package harness

import (
	"fmt"
	"github.com/robfig/revel"
	"go/build"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"text/template"
)

var importErrorPattern = regexp.MustCompile("import \"([^\"]+)\": cannot find package")

// Build the app:
// 1. Generate the the main.go file.
// 2. Run the appropriate "go build" command.
// Requires that revel.Init has been called previously.
// Returns the path to the built binary, and an error if there was a problem building it.
func Build() (app *App, compileError *revel.Error) {
	sourceInfo, compileError := ProcessSource(revel.CodePaths)
	if compileError != nil {
		return nil, compileError
	}

	// Add the db.import to the import paths.
	if dbImportPath, found := revel.Config.String("db.import"); found {
		sourceInfo.InitImportPaths = append(sourceInfo.InitImportPaths, dbImportPath)
	}

	tmpl := template.Must(template.New("").Parse(REGISTER_CONTROLLERS))
	registerControllerSource := revel.ExecuteTemplate(tmpl, map[string]interface{}{
		"Controllers":    sourceInfo.ControllerSpecs,
		"ValidationKeys": sourceInfo.ValidationKeys,
		"ImportPaths":    calcImportAliases(sourceInfo),
		"TestSuites":     sourceInfo.TestSuites,
	})

	// Create a fresh temp dir.
	tmpPath := path.Join(revel.AppPath, "tmp")
	err := os.RemoveAll(tmpPath)
	if err != nil {
		revel.ERROR.Println("Failed to remove tmp dir:", err)
	}
	err = os.Mkdir(tmpPath, 0777)
	if err != nil {
		revel.ERROR.Fatalf("Failed to make tmp directory: %v", err)
	}

	// Create the main.go file
	controllersFile, err := os.Create(path.Join(tmpPath, "main.go"))
	defer controllersFile.Close()
	if err != nil {
		revel.ERROR.Fatalf("Failed to create main.go: %v", err)
	}
	_, err = controllersFile.WriteString(registerControllerSource)
	if err != nil {
		revel.ERROR.Fatalf("Failed to write to main.go: %v", err)
	}

	// Read build config.
	buildTags := revel.Config.StringDefault("build.tags", "")

	// Build the user program (all code under app).
	// It relies on the user having "go" installed.
	goPath, err := exec.LookPath("go")
	if err != nil {
		revel.ERROR.Fatalf("Go executable not found in PATH.")
	}

	ctx := build.Default
	pkg, err := ctx.Import(revel.ImportPath, "", build.FindOnly)
	if err != nil {
		revel.ERROR.Fatalln("Failure importing", revel.ImportPath)
	}
	binName := path.Join(pkg.BinDir, path.Base(revel.BasePath))
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}

	gotten := make(map[string]struct{})
	for {
		buildCmd := exec.Command(goPath, "build",
			"-tags", buildTags,
			"-o", binName, path.Join(revel.ImportPath, "app", "tmp"))
		revel.TRACE.Println("Exec:", buildCmd.Args)
		output, err := buildCmd.CombinedOutput()

		// If the build succeeded, we're done.
		if err == nil {
			return NewApp(binName), nil
		}
		revel.TRACE.Println(string(output))

		// See if it was an import error that we can go get.
		matches := importErrorPattern.FindStringSubmatch(string(output))
		if matches == nil {
			return nil, newCompileError(output)
		}

		// Ensure we haven't already tried to go get it.
		pkgName := matches[1]
		if _, alreadyTried := gotten[pkgName]; alreadyTried {
			return nil, newCompileError(output)
		}
		gotten[pkgName] = struct{}{}

		// Execute "go get <pkg>"
		getCmd := exec.Command(goPath, "get", pkgName)
		revel.TRACE.Println("Exec:", getCmd.Args)
		getOutput, err := getCmd.CombinedOutput()
		if err != nil {
			revel.TRACE.Println(string(getOutput))
			return nil, newCompileError(output)
		}

		// Success getting the import, attempt to build again.
	}
	revel.ERROR.Fatalf("Not reachable")
	return nil, nil
}

// Looks through all the method args and returns a set of unique import paths
// that cover all the method arg types.
// Additionally, assign package aliases when necessary to resolve ambiguity.
func calcImportAliases(src *SourceInfo) map[string]string {
	aliases := make(map[string]string)
	typeArrays := [][]*TypeInfo{src.ControllerSpecs, src.TestSuites}
	for _, specs := range typeArrays {
		for _, spec := range specs {
			addAlias(aliases, spec.ImportPath, spec.PackageName)

			for _, methSpec := range spec.MethodSpecs {
				for _, methArg := range methSpec.Args {
					if methArg.ImportPath == "" {
						continue
					}

					addAlias(aliases, methArg.ImportPath, methArg.TypeExpr.PkgName)
				}
			}
		}
	}

	// Add the "InitImportPaths", with alias "_"
	for _, importPath := range src.InitImportPaths {
		if _, ok := aliases[importPath]; !ok {
			aliases[importPath] = "_"
		}
	}

	return aliases
}

func addAlias(aliases map[string]string, importPath, pkgName string) {
	alias, ok := aliases[importPath]
	if ok {
		return
	}
	alias = makePackageAlias(aliases, pkgName)
	aliases[importPath] = alias
}

func makePackageAlias(aliases map[string]string, pkgName string) string {
	i := 0
	alias := pkgName
	for containsValue(aliases, alias) {
		alias = fmt.Sprintf("%s%d", pkgName, i)
		i++
	}
	return alias
}

func containsValue(m map[string]string, val string) bool {
	for _, v := range m {
		if v == val {
			return true
		}
	}
	return false
}

// Parse the output of the "go build" command.
// Return a detailed Error.
func newCompileError(output []byte) *revel.Error {
	errorMatch := regexp.MustCompile(`(?m)^([^:#]+):(\d+):(\d+:)? (.*)$`).
		FindSubmatch(output)
	if errorMatch == nil {
		revel.ERROR.Println("Failed to parse build errors:\n", string(output))
		return &revel.Error{
			SourceType:  "Go code",
			Title:       "Go Compilation Error",
			Description: "See console for build error.",
		}
	}

	// Read the source for the offending file.
	var (
		relFilename    = string(errorMatch[1]) // e.g. "src/revel/sample/app/controllers/app.go"
		absFilename, _ = filepath.Abs(relFilename)
		line, _        = strconv.Atoi(string(errorMatch[2]))
		description    = string(errorMatch[4])
		compileError   = &revel.Error{
			SourceType:  "Go code",
			Title:       "Go Compilation Error",
			Path:        relFilename,
			Description: description,
			Line:        line,
		}
	)

	fileStr, err := revel.ReadLines(absFilename)
	if err != nil {
		compileError.MetaError = absFilename + ": " + err.Error()
		revel.ERROR.Println(compileError.MetaError)
		return compileError
	}

	compileError.SourceLines = fileStr
	return compileError
}

const REGISTER_CONTROLLERS = `package main

import (
	"flag"
	"reflect"
	"github.com/robfig/revel"{{range $k, $v := $.ImportPaths}}
	{{$v}} "{{$k}}"{{end}}
)

var (
	runMode    *string = flag.String("runMode", "", "Run mode.")
	port       *int    = flag.Int("port", 0, "By default, read from app.conf")
	importPath *string = flag.String("importPath", "", "Go Import Path for the app.")
	srcPath    *string = flag.String("srcPath", "", "Path to the source root.")

	// So compiler won't complain if the generated code doesn't reference reflect package...
	_ = reflect.Invalid
)

func main() {
	flag.Parse()
	revel.Init(*runMode, *importPath, *srcPath)
	revel.INFO.Println("Running revel server")
	{{range $i, $c := .Controllers}}
	revel.RegisterController((*{{index $.ImportPaths .ImportPath}}.{{.StructName}})(nil),
		[]*revel.MethodType{
			{{range .MethodSpecs}}&revel.MethodType{
				Name: "{{.Name}}",
				Args: []*revel.MethodArg{ {{range .Args}}
					&revel.MethodArg{Name: "{{.Name}}", Type: reflect.TypeOf((*{{index $.ImportPaths .ImportPath | .TypeExpr.TypeName}})(nil)) },{{end}}
				},
				RenderArgNames: map[int][]string{ {{range .RenderCalls}}
					{{.Line}}: []string{ {{range .Names}}
						"{{.}}",{{end}}
					},{{end}}
				},
			},
			{{end}}
		})
	{{end}}
	revel.DefaultValidationKeys = map[string]map[int]string{ {{range $path, $lines := .ValidationKeys}}
		"{{$path}}": { {{range $line, $key := $lines}}
			{{$line}}: "{{$key}}",{{end}}
		},{{end}}
	}
	revel.TestSuites = []interface{}{ {{range .TestSuites}}
		(*{{index $.ImportPaths .ImportPath}}.{{.StructName}})(nil),{{end}}
	}

	revel.Run(*port)
}
`
