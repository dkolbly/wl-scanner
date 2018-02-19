package main

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"text/template"
)

var source = flag.String("source", "", "Where to get the XML from")
var output = flag.String("output", "", "Where to put the output go file")

// xml types
type Protocol struct {
	XMLName    xml.Name    `xml:"protocol"`
	Name       string      `xml:"name,attr"`
	Copyright  string      `xml:"copyright"`
	Interfaces []Interface `xml:"interface"`
}

type Description struct {
	XMLName xml.Name `xml:"description"`
	Summary string   `xml:"summary,attr"`
	Text    string   `xml:",chardata"`
}

type Interface struct {
	XMLName     xml.Name    `xml:"interface"`
	Name        string      `xml:"name,attr"`
	Version     int         `xml:"version,attr"`
	Since       int         `xml:"since,attr"` // maybe in future versions
	Description Description `xml:"description"`
	Requests    []Request   `xml:"request"`
	Events      []Event     `xml:"event"`
	Enums       []Enum      `xml:"enum"`
}

type Request struct {
	XMLName     xml.Name    `xml:"request"`
	Name        string      `xml:"name,attr"`
	Type        string      `xml:"type,attr"`
	Since       int         `xml:"since,attr"`
	Description Description `xml:"description"`
	Args        []Arg       `xml:"arg"`
}

type Arg struct {
	XMLName   xml.Name `xml:"arg"`
	Name      string   `xml:"name,attr"`
	Type      string   `xml:"type,attr"`
	Interface string   `xml:"interface,attr"`
	Enum      string   `xml:"enum,attr"`
	AllowNull bool     `xml:"allow-null,attr"`
	Summary   string   `xml:"summary,attr"`
}

type Event struct {
	XMLName     xml.Name    `xml:"event"`
	Name        string      `xml:"name,attr"`
	Since       int         `xml:"since,attr"`
	Description Description `xml:"description"`
	Args        []Arg       `xml:"arg"`
}

type Enum struct {
	XMLName     xml.Name    `xml:"enum"`
	Name        string      `xml:"name,attr"`
	BitField    bool        `xml:"bitfield,attr"`
	Description Description `xml:"description"`
	Entries     []Entry     `xml:"entry"`
}

type Entry struct {
	XMLName xml.Name `xml:"entry"`
	Name    string   `xml:"name,attr"`
	Value   string   `xml:"value,attr"`
	Summary string   `xml:"summary,attr"`
}

// go types
type (
	GoInterface struct {
		Name        string
		WlInterface Interface
		Requests    []GoRequest
		Events      []GoEvent
		Enums       []GoEnum
	}

	GoRequest struct {
		Name           string
		IfaceName      string
		Params         string
		Returns        string
		Args           string
		HasNewId       bool
		NewIdInterface string
		Order          int
		Summary        string
		Description    string
	}

	GoEvent struct {
		Name      string
		IfaceName string
		PName     string
		Args      []GoArg
	}

	GoArg struct {
		Name      string
		Type      string
		PName     string
		BufMethod string
	}

	GoEnum struct {
		Name      string
		IfaceName string
		Entries   []GoEntry
	}

	GoEntry struct {
		Name  string
		Value string
	}
)

var (
	wlTypes map[string]string = map[string]string{
		"int":    "int32",
		"uint":   "uint32",
		"string": "string",
		"fd":     "uintptr",
		"fixed":  "float32",
		"array":  "[]int32",
	}

	// sync with event.go
	bufTypesMap map[string]string = map[string]string{
		"int32":   "Int32()",
		"uint32":  "Uint32()",
		"string":  "String()",
		"float32": "Float32()",
		"[]int32": "Array()",
		"uintptr": "FD()",
	}

	wlNames    map[string]string
	fileBuffer = &bytes.Buffer{}
)

func sourceData() io.Reader {
	if *source == "" {
		log.Fatal("Must specify a -source")
	}

	if strings.HasPrefix(*source, "http:") || strings.HasPrefix(*source, "https:") {
		resp, err := http.Get(*source)
		if err != nil {
			log.Fatal(err)
		}
		return resp.Body
	} else {
		f, err := os.Open(*source)
		if err != nil {
			log.Fatal(err)
		}
		return f
	}
}

func main() {
	log.SetFlags(0)
	flag.Parse()

	dest := *output
	if dest == "" {
		log.Fatal("Must specify -output")
	}

	var protocol Protocol

	file := sourceData()

	err := decodeWlXML(file, &protocol)
	if err != nil {
		log.Fatal(err)
	}

	wlNames = make(map[string]string)

	if protocol.Name != "wayland" {
		for _, inherit := range inheritedNames {
			wlNames[inherit] = CamelCase(inherit)
		}
	}

	// required for request and event parameters
	for _, iface := range protocol.Interfaces {
		caseAndRegister(iface.Name)
	}

	fmt.Fprintf(fileBuffer, "// generated by wl-scanner\n// https://github.com/dkolbly/wl-scanner\n")
	fmt.Fprintf(fileBuffer, "// from: %s\n", *source)
	fmt.Fprintln(fileBuffer, "package wl")
	fmt.Fprintln(fileBuffer, "import \"sync\"")

	for _, iface := range protocol.Interfaces {
		goIface := GoInterface{
			Name:        wlNames[iface.Name],
			WlInterface: iface,
		}

		goIface.ProcessEvents()
		goIface.Constructor()
		goIface.ProcessRequests()
		goIface.ProcessEnums()
	}

	out, err := os.Create(dest)
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()

	fileBuffer.WriteTo(out)

	fmtFile()
}

func decodeWlXML(file io.Reader, prot *Protocol) error {
	err := xml.NewDecoder(file).Decode(&prot)
	if err != nil {
		return fmt.Errorf("Cannot decode wayland.xml: %s", err)
	}
	return nil
}

// register names to map
func caseAndRegister(wlName string) string {
	var orj string = wlName
	wlName = CamelCase(wlName)
	wlNames[orj] = wlName
	return wlName
}

func executeTemplate(name string, tpl string, data interface{}) {
	tmpl := template.Must(template.New(name).Parse(tpl))
	err := tmpl.Execute(fileBuffer, data)
	if err != nil {
		log.Fatal(err)
	}
}

func (i *GoInterface) Constructor() {
	executeTemplate("InterfaceTypeTemplate", ifaceTypeTemplate, i)
	executeTemplate("InterfaceConstructorTemplate", ifaceConstructorTemplate, i)
}

func (i *GoInterface) ProcessRequests() {
	for order, wlReq := range i.WlInterface.Requests {
		var (
			returns         []string
			params          []string
			sendRequestArgs []string // for sendRequest
		)

		req := GoRequest{
			Name:        CamelCase(wlReq.Name),
			IfaceName:   i.Name,
			Order:       order,
			Summary:     wlReq.Description.Summary,
			Description: reflow(wlReq.Description.Text),
		}

		/* TODO request kodlarını sabit olarak tanımla
		reqCodeName := strings.ToTitle(fmt.Sprintf("_%s_%s", i.Name , req.Name)) // first _ for not export constant
		"%s = %d", reqCodeName, order)
		*/

		for _, arg := range wlReq.Args {
			if arg.Type == "new_id" {
				if arg.Interface != "" {
					newIdIface := wlNames[arg.Interface]
					req.NewIdInterface = newIdIface
					sendRequestArgs = append(params, "Proxy(ret)")
					req.HasNewId = true

					returns = append(returns, "*"+newIdIface)
				} else { //special for registry.Bind
					sendRequestArgs = append(sendRequestArgs, "iface")
					sendRequestArgs = append(sendRequestArgs, "version")
					sendRequestArgs = append(sendRequestArgs, arg.Name)

					params = append(params, "iface string")
					params = append(params, "version uint32")
					params = append(params, fmt.Sprintf("%s Proxy", arg.Name))
				}
			} else if arg.Type == "object" && arg.Interface != "" {
				paramTypeName := wlNames[arg.Interface]
				params = append(params, fmt.Sprintf("%s *%s", arg.Name, paramTypeName))
				sendRequestArgs = append(sendRequestArgs, arg.Name)
				/*} else if arg.Type == "uint" && arg.Enum != "" {
					params = append(params, fmt.Sprintf("%s %s", arg.Name, enumArgName(ifaceName, arg.Enum)))
				}*/
			} else {
				sendRequestArgs = append(sendRequestArgs, arg.Name)
				params = append(params, fmt.Sprintf("%s %s", arg.Name, wlTypes[arg.Type]))
			}
		}

		req.Params = strings.Join(params, ",")

		if len(sendRequestArgs) > 0 {
			req.Args = "," + strings.Join(sendRequestArgs, ",")
		}

		if len(returns) > 0 { // ( ret , error )
			req.Returns = fmt.Sprintf("(%s , error)", strings.Join(returns, ","))
		} else { // returns only error
			req.Returns = "error"
		}

		executeTemplate("RequestTemplate", requestTemplate, req)
		i.Requests = append(i.Requests, req)
	}
}

func (i *GoInterface) ProcessEvents() {
	// Event struct types
	for _, wlEv := range i.WlInterface.Events {
		ev := GoEvent{
			Name:      CamelCase(wlEv.Name),
			PName:     snakeCase(wlEv.Name),
			IfaceName: i.Name,
		}

		for _, arg := range wlEv.Args {
			goarg := GoArg{
				Name:  CamelCase(arg.Name),
				PName: snakeCase(arg.Name),
			}
			if t, ok := wlTypes[arg.Type]; ok { // if basic type
				bufMethod, ok := bufTypesMap[t]
				if !ok {
					log.Printf("%s not registered", t)
				} else {
					goarg.BufMethod = bufMethod
				}
				/*
					if arg.Type == "uint" && arg.Enum != "" { // enum type
						enumTypeName := ifaceName + CamelCase(arg.Enum)
						fmt.Fprintf(&eventBuffer, "%s %s\n", CamelCase(arg.Name), enumTypeName)
					} else {
						fmt.Fprintf(&eventBuffer, "%s %s\n", CamelCase(arg.Name), t)
					}*/
				goarg.Type = t
			} else { // interface type
				if (arg.Type == "object" || arg.Type == "new_id") && arg.Interface != "" {
					t = "*" + wlNames[arg.Interface]
					goarg.BufMethod = fmt.Sprintf("Proxy(p.Context()).(%s)", t)
				} else {
					t = "Proxy"
					goarg.BufMethod = "Proxy(p.Context())"
				}
				goarg.Type = t
			}

			ev.Args = append(ev.Args, goarg)
		}

		executeTemplate("EventTemplate", eventTemplate, ev)
		executeTemplate("AddRemoveHandlerTemplate", ifaceAddRemoveHandlerTemplate, ev)

		i.Events = append(i.Events, ev)
	}

	if len(i.Events) > 0 {
		executeTemplate("InterfaceDispatchTemplate", ifaceDispatchTemplate, i)
	}
}

func (i *GoInterface) ProcessEnums() {
	// Enums - Constants
	for _, wlEnum := range i.WlInterface.Enums {
		goEnum := GoEnum{
			Name:      CamelCase(wlEnum.Name),
			IfaceName: i.Name,
		}

		for _, wlEntry := range wlEnum.Entries {
			goEntry := GoEntry{
				Name:  CamelCase(wlEntry.Name),
				Value: wlEntry.Value,
			}
			goEnum.Entries = append(goEnum.Entries, goEntry)
		}

		executeTemplate("InterfaceEnumsTemplate", ifaceEnums, goEnum)
	}
}

/*
func enumArgName(ifaceName, enumName string) string {
	if strings.Index(enumName, ".") == -1 {
		return ifaceName + CamelCase(enumName)
	}

	parts := strings.Split(enumName, ".")
	if len(parts) != 2 {
		log.Fatalf("enum args must be \"interface.enum\" format: we get %s",enumName)
	}
	return CamelCase(parts[0]) + CamelCase(parts[1])
}
*/

func CamelCase(wlName string) string {
	if strings.HasPrefix(wlName, "wl_") {
		wlName = strings.TrimPrefix(wlName, "wl_")
	}

	// replace all "_" chars to " " chars
	wlName = strings.Replace(wlName, "_", " ", -1)

	// Capitalize first chars
	wlName = strings.Title(wlName)

	// remove all spaces
	wlName = strings.Replace(wlName, " ", "", -1)

	return wlName
}

func snakeCase(wlName string) string {
	if strings.HasPrefix(wlName, "wl_") {
		wlName = strings.TrimPrefix(wlName, "wl_")
	}

	// replace all "_" chars to " " chars
	wlName = strings.Replace(wlName, "_", " ", -1)
	parts := strings.Split(wlName, " ")
	for i, p := range parts {
		if i == 0 {
			continue
		}
		parts[i] = strings.Title(p)
	}

	return strings.Join(parts, "")
}

func fmtFile() {
	goex, err := exec.LookPath("go")
	if err != nil {
		log.Printf("go executable cannot found run \"go fmt %s\" yourself: %s", *output, err)
		return
	}

	cmd := exec.Command(goex, "fmt", *output)
	er2 := cmd.Run()
	if er2 != nil {
		log.Fatalf("Cannot run cmd: %s", er2)
	}
}

// templates
var (
	ifaceTypeTemplate = `
type {{.Name}} struct {
	BaseProxy
	{{- if gt (len .Events) 0 }}
	mu sync.RWMutex
	{{- end}}

	{{- range .Events}}
	{{.PName}}Handlers []Handler
	{{- end}}
}
`
	ifaceConstructorTemplate = `
func New{{.Name}}(ctx *Context) *{{.Name}} {
	ret := new({{.Name}})
	ctx.register(ret)
	return ret
}
`
	ifaceAddRemoveHandlerTemplate = `
func (p *{{.IfaceName}}) Add{{.Name}}Handler(h Handler) {
	if h != nil {
		p.mu.Lock()
		p.{{.PName}}Handlers = append(p.{{.PName}}Handlers , h)
		p.mu.Unlock()
	}
}

func (p *{{.IfaceName}}) Remove{{.Name}}Handler(h Handler) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i , e := range p.{{.PName}}Handlers {
		if e == h {
			p.{{.PName}}Handlers = append(p.{{.PName}}Handlers[:i] , p.{{.PName}}Handlers[i+1:]...)
			break
		}
	}
}
`

	requestTemplate = `
// {{.Name}} will {{.Summary}}.
//
{{.Description}}func (p *{{.IfaceName}}) {{.Name}}({{.Params}}) {{.Returns}} {
	{{- if .HasNewId}}
	ret := New{{.NewIdInterface}}(p.Context())
	return ret , p.Context().sendRequest(p,{{.Order}}{{.Args}})
	{{- else}}
	return p.Context().sendRequest(p,{{.Order}}{{.Args}})
	{{- end}}
}
`

	eventTemplate = `
type {{.IfaceName}}{{.Name}}Event struct {
	{{- range .Args }}
	{{.Name}} {{.Type}}
	{{- end }}
}
`
	ifaceDispatchTemplate = `
func (p *{{.Name}}) Dispatch(event *Event) {
	{{- $ifaceName := .Name }}
	switch event.opcode {
	{{- range $i , $event := .Events }}
	case {{$i}}:
		if len(p.{{.PName}}Handlers) > 0 {
			ev := {{$ifaceName}}{{.Name}}Event{}
			{{- range $event.Args}}
			ev.{{.Name}} = event.{{.BufMethod}}
			{{- end}}
			p.mu.RLock()
			for _, h := range p.{{.PName}}Handlers {
				h.Handle(ev)
			}
			p.mu.RUnlock()
		}
	{{- end}}
	}
}
`
	ifaceEnums = `
const (
	{{- $ifaceName := .IfaceName }}
	{{- $enumName := .Name }}
	{{- range .Entries}}
	{{$ifaceName}}{{$enumName}}{{.Name}} = {{.Value}}
	{{- end}}
)
`
)

var inheritedNames = []string{
	"wl_display",
	"wl_registry",
	"wl_callback",
	"wl_compositor",
	"wl_shm_pool",
	"wl_shm",
	"wl_buffer",
	"wl_data_offer",
	"wl_data_source",
	"wl_data_device",
	"wl_data_device_manager",
	"wl_shell",
	"wl_shell_surface",
	"wl_surface",
	"wl_seat",
	"wl_pointer",
	"wl_keyboard",
	"wl_touch",
	"wl_output",
	"wl_region",
	"wl_subcompositor",
	"wl_subsurface",
}

func reflow(text string) string {
	ret := ""
	for _, line := range strings.Split(text, "\n") {
		ret = ret + "// " + strings.TrimSpace(line) + "\n"
	}
	return ret
}
