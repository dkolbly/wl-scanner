package main

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type (
	Protocol struct {
		XMLName    xml.Name    `xml:"protocol"`
		Name       string      `xml:"name,attr"`
		Copyright  string      `xml:"copyright"`
		Interfaces []Interface `xml:"interface"`
	}

	Description struct {
		XMLName     xml.Name `xml:"description"`
		Summary     string   `xml:"summary,attr"`
		Description string   `xml:"description"`
	}

	Interface struct {
		XMLName     xml.Name    `xml:"interface"`
		Name        string      `xml:"name,attr"`
		Version     int         `xml:"version,attr"`
		Since       int         `xml:"since,attr"` // maybe in future versions
		Description Description `xml:"description"`
		Requests    []Request   `xml:"request"`
		Events      []Event     `xml:"event"`
		Enums       []Enum      `xml:"enum"`
	}

	Request struct {
		XMLName     xml.Name    `xml:"request"`
		Name        string      `xml:"name,attr"`
		Type        string      `xml:"type,attr"`
		Since       int         `xml:"since,attr"`
		Description Description `xml:"description"`
		Args        []Arg       `xml:"arg"`
	}

	Arg struct {
		XMLName   xml.Name `xml:"arg"`
		Name      string   `xml:"name,attr"`
		Type      string   `xml:"type,attr"`
		Interface string   `xml:"interface,attr"`
		Enum      string   `xml:"enum,attr"`
		AllowNull bool     `xml:"allow-null,attr"`
		Summary   string   `xml:"summary,attr"`
	}

	Event struct {
		XMLName     xml.Name    `xml:"event"`
		Name        string      `xml:"name,attr"`
		Since       int         `xml:"since,attr"`
		Description Description `xml:"description"`
		Args        []Arg       `xml:"arg"`
	}

	Enum struct {
		XMLName     xml.Name    `xml:"enum"`
		Name        string      `xml:"name,attr"`
		BitField    bool        `xml:"bitfield,attr"`
		Description Description `xml:"description"`
		Entries     []Entry     `xml:"entry"`
	}

	Entry struct {
		XMLName xml.Name `xml:"entry"`
		Name    string   `xml:"name,attr"`
		Value   string   `xml:"value,attr"`
		Summary string   `xml:"summary,attr"`
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

	wlNames        map[string]string
	constBuffer    bytes.Buffer
	ifaceBuffer    bytes.Buffer
	reqCodesBuffer bytes.Buffer

	overwrite = flag.Bool("o", false, "Overwrite existing client.go file")
	develXml  = flag.Bool("dev", false, "Get development version of wayland.xml from repository")
)

func init() {
	flag.Parse()
	log.SetFlags(0)
}

func main() {
	var xmlFile *os.File

	if *develXml {
		file, err := getDevelXml()
		if err != nil {
			file.Close()
			log.Fatalf("Error while reading xml file : %s", err)
		}
		xmlFile = file
		xmlFile.Seek(0, 0)
	} else {
		xmlFilePath, err := filepath.Abs("wayland.xml")
		if err != nil {
			log.Fatalf("Cannot find wayland.xml: %s", err)
		}

		file, err := os.Open(xmlFilePath)
		if err != nil {
			log.Fatalf("Cannot open wayland.xml:%s", err)
		}
		xmlFile = file
	}

	defer xmlFile.Close()

	var protocol Protocol
	if err := xml.NewDecoder(xmlFile).Decode(&protocol); err != nil {
		log.Fatalf("Cannot decode wayland.xml : %s", err)
	}

	wlNames = make(map[string]string)

	fmt.Fprint(&constBuffer, "package wl")

	for _, iface := range protocol.Interfaces {
		//required for arg type's determine
		caseAndRegister(iface.Name)
	}

	fmt.Fprint(&reqCodesBuffer, "\n//Interface Request Codes\n") // request codes
	fmt.Fprint(&reqCodesBuffer, "\nconst (\n")                   // request codes

	for _, iface := range protocol.Interfaces {
		eventBuffer, eventNames := interfaceEvents(iface)
		eventBuffer.WriteTo(&ifaceBuffer)

		interfaceTypes(iface, eventNames)
		interfaceConstructor(iface, eventNames)
		interfaceRequests(iface)
		interfaceEnums(iface)
	}

	fmt.Fprint(&reqCodesBuffer, ")") // request codes end

	// if file exists
	if _, err := os.Stat("client.go"); err == nil {
		if !*overwrite {
			log.Print("client.go exists if you want to overwrite try -o flag")
			return
		}
	}

	file, err := os.Create("client.go")
	if err != nil {
		log.Fatalf("Cannot create file: %s", err)
	}

	constBuffer.WriteTo(file)
	reqCodesBuffer.WriteTo(file)
	ifaceBuffer.WriteTo(file)

	file.Close()

	// go fmt file
	fmtFile()
}

// register names to map
func caseAndRegister(wlName string) string {
	var orj string = wlName
	wlName = CamelCase(wlName)
	wlNames[orj] = wlName
	return wlName
}

func enumArgName(ifaceName, enumName string) string {
	if strings.Index(enumName, ".") == -1 {
		return ifaceName + CamelCase(enumName)
	} else {
		parts := strings.Split(enumName, ".")
		if len(parts) != 2 {
			log.Fatal("enum args must be \"interface.enum\" format")
		}
		return CamelCase(parts[0]) + CamelCase(parts[1])
	}
}

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

func interfaceConstructor(iface Interface, eventNames []string) {
	ifaceName := wlNames[iface.Name]

	// interface constructor
	fmt.Fprintf(&ifaceBuffer, "\nfunc New%s(conn *Connection) *%s {\n", ifaceName, ifaceName)
	fmt.Fprintf(&ifaceBuffer, "ret := new(%s)\n", ifaceName)
	for _, evName := range eventNames {
		fmt.Fprintf(&ifaceBuffer, "ret.%sChan = make(chan %s%sEvent)\n", evName, ifaceName, evName)
	}

	fmt.Fprint(&ifaceBuffer, "conn.Register(ret)\n")
	fmt.Fprint(&ifaceBuffer, "return ret\n")
	fmt.Fprint(&ifaceBuffer, "}\n")
}

func interfaceTypes(iface Interface, eventNames []string) {
	ifaceName := wlNames[iface.Name]
	// interface type definition
	fmt.Fprintf(&ifaceBuffer, "\ntype %s struct {\n", ifaceName)
	fmt.Fprint(&ifaceBuffer, "BaseProxy\n")
	for _, evName := range eventNames {
		fmt.Fprintf(&ifaceBuffer, "%sChan chan %s%sEvent\n", evName, ifaceName, evName)
	}
	fmt.Fprint(&ifaceBuffer, "}\n")
}

func interfaceRequests(iface Interface) {
	ifaceName := wlNames[iface.Name]

	// interface method definitions (requests)
	// order used for request identification
	for order, req := range iface.Requests {
		reqName := CamelCase(req.Name)
		reqCodeName := strings.ToTitle(fmt.Sprintf("_%s_%s", ifaceName, reqName)) // first _ for not export constant
		fmt.Fprintf(&reqCodesBuffer, "%s = %d\n", reqCodeName, order)

		fmt.Fprintf(&ifaceBuffer, "\nfunc (p *%s) %s(", ifaceName, reqName)
		// get args buffer
		requestArgs(ifaceName, req).WriteTo(&ifaceBuffer)

		fmt.Fprint(&ifaceBuffer, ")") // close the args

		// get returns buffer
		requestRets(req).WriteTo(&ifaceBuffer)
		fmt.Fprint(&ifaceBuffer, "{\n")

		// get method body
		requestBody(req, reqCodeName).WriteTo(&ifaceBuffer)

		fmt.Fprint(&ifaceBuffer, "\n}\n")
	}
}

func interfaceEnums(iface Interface) {
	ifaceName := wlNames[iface.Name]

	// Enums - Constants
	for _, enum := range iface.Enums {
		enumName := caseAndRegister(enum.Name)
		constTypeName := ifaceName + enumName
		fmt.Fprintf(&constBuffer, "\ntype %s uint32\n", constTypeName) // enums are uint
		fmt.Fprint(&constBuffer, "const (\n")
		for _, entry := range enum.Entries {
			entryName := caseAndRegister(entry.Name)
			constName := ifaceName + enumName + entryName
			fmt.Fprintf(&constBuffer, "%s %s = %s\n", constName, constTypeName, entry.Value)
		}
		fmt.Fprint(&constBuffer, ")\n")
	}
}

func interfaceEvents(iface Interface) (bytes.Buffer, []string) {
	var (
		eventBuffer bytes.Buffer
		eventNames  []string
		ifaceName   = wlNames[iface.Name]
	)

	// Event struct types
	for _, event := range iface.Events {
		eventName := caseAndRegister(event.Name)
		fmt.Fprintf(&eventBuffer, "\ntype %s%sEvent struct {\n", ifaceName, eventName)
		for _, arg := range event.Args {
			if t, ok := wlTypes[arg.Type]; ok { // if basic type
				if arg.Type == "uint" && arg.Enum != "" { // enum type
					enumTypeName := ifaceName + CamelCase(arg.Enum)
					fmt.Fprintf(&eventBuffer, "%s %s\n", CamelCase(arg.Name), enumTypeName)
				} else {
					fmt.Fprintf(&eventBuffer, "%s %s\n", CamelCase(arg.Name), t)
				}
			} else { // interface type
				if (arg.Type == "object" || arg.Type == "new_id") && arg.Interface != "" {
					t = "*" + wlNames[arg.Interface]
				} else {
					t = "Proxy"
				}
				fmt.Fprintf(&eventBuffer, "%s %s\n", CamelCase(arg.Name), t)
			}
		}

		eventNames = append(eventNames, eventName)
		fmt.Fprint(&eventBuffer, "}\n")
	}

	return eventBuffer, eventNames
}

func requestArgs(ifaceName string, req Request) *bytes.Buffer {
	var (
		args       []string
		argsBuffer bytes.Buffer
	)

	for _, arg := range req.Args {
		// special type, for example registry.bind
		if arg.Type == "new_id" {
			if arg.Interface == "" {
				args = append(args, "iface string")
				args = append(args, "version uint32")
				args = append(args, fmt.Sprintf("%s Proxy", arg.Name))
			} else {
				continue
			}
		} else if arg.Type == "object" && arg.Interface != "" {
			argTypeName := wlNames[arg.Interface]
			args = append(args, fmt.Sprintf("%s *%s", arg.Name, argTypeName))
		} else if arg.Type == "uint" && arg.Enum != "" {
			args = append(args, fmt.Sprintf("%s %s", arg.Name, enumArgName(ifaceName, arg.Enum)))
		} else {
			args = append(args, fmt.Sprintf("%s %s", arg.Name, wlTypes[arg.Type]))
		}
	}

	fmt.Fprint(&argsBuffer, strings.Join(args, ","))

	return &argsBuffer
}

func requestRets(req Request) *bytes.Buffer {
	var (
		rets       []string
		retsBuffer bytes.Buffer
	)

	for _, arg := range req.Args {
		if arg.Type == "new_id" && arg.Interface != "" {
			retTypeName := wlNames[arg.Interface]
			rets = append(rets, fmt.Sprintf("*%s", retTypeName))
		}
	}

	// all request have an error return
	rets = append(rets, "error")

	retstr := strings.Join(rets, ",")

	if len(rets) > 1 {
		fmt.Fprintf(&retsBuffer, "( %s )", retstr)
	} else {
		fmt.Fprint(&retsBuffer, retstr)
	}

	return &retsBuffer
}

func requestBody(req Request, reqCodeName string) *bytes.Buffer {
	var (
		params       []string
		bodyBuffer   bytes.Buffer
		paramsBuffer bytes.Buffer
		hasRet       string
	)

	for _, arg := range req.Args {
		if arg.Type == "new_id" {
			if arg.Interface != "" {
				retTypeName := wlNames[arg.Interface]
				fmt.Fprintf(&bodyBuffer, "ret := New%s(p.Connection())\n", retTypeName)
				params = append(params, "Proxy(ret)")
				hasRet = "ret,"
			} else {
				params = append(params, "iface")
				params = append(params, "version")
				params = append(params, arg.Name)
			}
		} else {
			params = append(params, arg.Name)
		}
	}

	for _, param := range params {
		fmt.Fprintf(&paramsBuffer, ",%s", param)
	}

	fmt.Fprintf(&bodyBuffer, "return %s p.Connection().SendRequest(p,%s%s)", hasRet, reqCodeName, paramsBuffer.String())

	return &bodyBuffer
}

func getDevelXml() (*os.File, error) {
	url := "https://cgit.freedesktop.org/wayland/wayland/plain/protocol/wayland.xml"
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("http get error")
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Cannot get wayland.xml StatusCode != StatusOK")
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Cannot read response body: %s", err)
	} else {
		file, err := ioutil.TempFile("", "devel_wayland_xml")
		if err != nil {
			return nil, fmt.Errorf("Cannot create temp file: %s", err)
		} else {
			file.Write(body)
			return file, nil
		}
	}
}

func fmtFile() {
	goex, err := exec.LookPath("go")
	if err != nil {
		log.Printf("go executable cannot found run \"go fmt client.go\" yourself: %s", err)
	} else {
		cmd := exec.Command(goex, "fmt", "client.go")
		err := cmd.Run()
		if err != nil {
			log.Fatalf("Cannot run cmd : %s", err)
		}
	}
}
