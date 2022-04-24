package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/TobiasYin/go-lsp/logs"
	"github.com/TobiasYin/go-lsp/lsp"
	"github.com/TobiasYin/go-lsp/lsp/defines"
	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/interrupts"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/metrics"
	"k8s.io/test-infra/prow/simplifypath"

	"github.com/openshift/ci-tools/pkg/load/agents"
)

type options struct {
	configPath             string
	registryPath           string
	logLevel               string
	address                string
	port                   int
	uiAddress              string
	uiPort                 int
	gracePeriod            time.Duration
	instrumentationOptions flagutil.InstrumentationOptions
}

var (
	configresolverMetrics = metrics.NewMetrics("configresolver")
)

var logPath *string

func init() {
	var logger *log.Logger
	defer func() {
		logs.Init(logger)
	}()
	logPath = flag.String("logs", "", "logs file path")
	if logPath == nil || *logPath == "" {
		logger = log.New(os.Stderr, "", 0)
		return
	}
	p := *logPath
	f, err := os.Open(p)
	if err == nil {
		logger = log.New(f, "", 0)
		return
	}
	f, err = os.Create(p)
	if err == nil {
		logger = log.New(f, "", 0)
		return
	}
	panic(fmt.Sprintf("logs init error: %v", *logPath))
}

func gatherOptions() (options, error) {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&o.configPath, "config", "", "Path to config dirs")
	fs.StringVar(&o.registryPath, "registry", "", "Path to registry dirs")
	fs.StringVar(&o.logLevel, "log-level", "info", "Level at which to log output.")
	fs.StringVar(&o.address, "address", ":8080", "DEPRECATED: Address to run server on")
	fs.StringVar(&o.uiAddress, "ui-address", ":8082", "DEPRECATED: Address to run the registry UI on")
	fs.IntVar(&o.port, "port", 8080, "Port to run server on")
	fs.IntVar(&o.uiPort, "ui-port", 8082, "Port to run the registry UI on")
	fs.DurationVar(&o.gracePeriod, "gracePeriod", time.Second*10, "Grace period for server shutdown")
	_ = fs.Duration("cycle", time.Minute*2, "Legacy flag kept for compatibility. Does nothing")
	o.instrumentationOptions.AddFlags(fs)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return o, fmt.Errorf("failed to parse flags: %w", err)
	}
	return o, nil
}

func validateOptions(o options) error {
	_, err := logrus.ParseLevel(o.logLevel)
	if err != nil {
		return fmt.Errorf("invalid --log-level: %w", err)
	}
	return o.instrumentationOptions.Validate(false)
}

func getConfigGeneration(agent agents.ConfigAgent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "%d", agent.GetGeneration())
	}
}

func getRegistryGeneration(agent agents.RegistryAgent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "%d", agent.GetGeneration())
	}
}

// l and v keep the tree legible
func l(fragment string, children ...simplifypath.Node) simplifypath.Node {
	return simplifypath.L(fragment, children...)
}

func strPtr(str string) *string {
	return &str
}

func ReadFile(filename defines.DocumentUri) ([]string, error) {
	enEscapeUrl, _ := url.QueryUnescape(string(filename))
	data, err := ioutil.ReadFile(enEscapeUrl[6:])
	if err != nil {
		return nil, err
	}
	content := string(data)
	line := strings.Split(content, "\n")
	return line, nil
}

func server() *lsp.Server {
	server := lsp.NewServer(
		&lsp.Options{
			Network:            "",
			Address:            "",
			CompletionProvider: &defines.CompletionOptions{},
			HoverProvider:      &defines.HoverOptions{},
		},
	)

	var configAgent *agents.ConfigAgent
	var registryAgent *agents.RegistryAgent
	var registryPath string

	server.OnInitialized(func(ctx context.Context, req *defines.InitializeParams) error {
		return nil
	})

	server.OnInitialize(func(ctx context.Context, req *defines.InitializeParams) (*defines.InitializeResult, *defines.InitializeError) {
		errNoRetry := &defines.InitializeError{Retry: false}
		folders, ok := req.WorkspaceFolders.([]interface{})

		if !ok {
			log.Println("Bad WorkspaceFolders type")
			return nil, errNoRetry
		}

		if len(folders) != 1 {
			return nil, errNoRetry
		}

		workspaceMap, ok := folders[0].(map[string]interface{})

		if !ok {
			log.Println("Bad workspace type")
			return nil, errNoRetry
		}

		uri, ok := workspaceMap["uri"]

		if !ok {
			log.Println("Workspace needs to have a URI")
			return nil, errNoRetry
		}

		folder, ok := uri.(string)

		if !ok {
			log.Println("URI has to be a string")
			return nil, errNoRetry
		}

		folder = strings.TrimPrefix(folder, "file://")

		if _, err := os.Stat(folder); err != nil {
			if os.IsNotExist(err) {
				log.Printf("workspace points to a nonexistent directory: %v", err)
			}
			log.Printf("Error getting stat info for --config directory: %v", err)
			return nil, errNoRetry
		}

		configPath := path.Join(folder, "ci-operator", "config")
		registryPath = path.Join(folder, "ci-operator", "step-registry")

		initConfigAgent, err := agents.NewConfigAgent(configPath, agents.WithConfigMetrics(configresolverMetrics.ErrorRate))
		if err != nil {
			log.Printf("Failed to get config agent: %v", err)
			return nil, errNoRetry
		}
		configAgent = &initConfigAgent

		initRegistryAgent, err := agents.NewRegistryAgent(registryPath,
			agents.WithRegistryMetrics(configresolverMetrics.ErrorRate),
			agents.WithRegistryFlat(false))
		if err != nil {
			log.Printf("Failed to get registry agent: %v", err)
			return nil, errNoRetry
		}
		registryAgent = &initRegistryAgent

		init := builtinInitialize(ctx, req)

		return &init, nil
	})

	server.OnDefinition(func(ctx context.Context, req *defines.DefinitionParams) (*[]defines.LocationLink, error) {
		yamlFile, err := ioutil.ReadFile(strings.TrimPrefix(string(req.TextDocument.Uri), "file://"))

		if err != nil {
			log.Printf("yamlFile.Get err   #%v ", err)
		}

		yamlFileStr := string(yamlFile)

		lines := strings.Split(yamlFileStr, "\n")
		line := lines[req.Position.Line]

		keyVal := strings.Split(line, ":")
		key := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(keyVal[0]), "-"))
		val := strings.TrimSpace(keyVal[1])

		comps := strings.Split(val, "-")

		var extension string
		switch key {
		case "workflow", "chain", "ref":
			extension = ".yaml"
		case "commands":
			comps = comps[:len(comps) - 1]
			extension = ".sh"
		default:
			return nil, nil
		}

		directory := path.Join(append([]string{registryPath}, comps...)...)
		filename := strings.Join(append(comps, key), "-") + extension
		location := defines.LocationLink{
			TargetUri: defines.DocumentUri("file://" + path.Join(directory, filename)),
		}
		locations := []defines.LocationLink{location}
		return &locations, nil
	})

	server.OnHover(func(ctx context.Context, req *defines.HoverParams) (result *defines.Hover, err error) {
		logs.Println("hover: ", req, configAgent, registryAgent)
		return &defines.Hover{Contents: defines.MarkupContent{Kind: defines.MarkupKindPlainText, Value: "hello world"}}, nil
	})

	server.OnCompletion(func(ctx context.Context, req *defines.CompletionParams) (result *[]defines.CompletionItem, err error) {
		logs.Println("completion: ", req)
		d := defines.CompletionItemKindText
		return &[]defines.CompletionItem{{
			Label:      "code",
			Kind:       &d,
			InsertText: strPtr("Hello"),
		}}, nil
	})

	server.OnDocumentFormatting(func(ctx context.Context, req *defines.DocumentFormattingParams) (result *[]defines.TextEdit, err error) {
		logs.Println("format: ", req)
		_, err = ReadFile(req.TextDocument.Uri)
		if err != nil {
			return nil, err
		}
		res := []defines.TextEdit{}

		return &res, nil
	})

	return server
}

func main() {
	logrusutil.ComponentInit()
	o, err := gatherOptions()
	if err != nil {
		logrus.WithError(err).Fatal("failed go gather options")
	}
	if err := validateOptions(o); err != nil {
		logrus.Fatalf("invalid options: %v", err)
	}
	level, _ := logrus.ParseLevel(o.logLevel)
	logrus.SetLevel(level)

	server := server()
	server.Run()

	interrupts.WaitForGracefulShutdown()
}

func builtinInitialize(ctx context.Context, req *defines.InitializeParams) defines.InitializeResult {
	resp := defines.InitializeResult{}
	// resp.Capabilities.TextDocumentSync = defines.TextDocumentSyncKindNone
	resp.Capabilities.CompletionProvider = &defines.CompletionOptions{
		TriggerCharacters: &[]string{"-"},
	}
	resp.Capabilities.HoverProvider = true
	resp.Capabilities.DefinitionProvider = true

	//if m.Opt.SignatureHelpProvider != nil {
	//	resp.Capabilities.SignatureHelpProvider = m.Opt.SignatureHelpProvider
	//} else if m.onSignatureHelp != nil {
	//	resp.Capabilities.SignatureHelpProvider = &defines.SignatureHelpOptions{
	//		TriggerCharacters: &[]string{"(", ","},
	//	}
	//}
	//if m.Opt.DeclarationProvider != nil {
	//	resp.Capabilities.DeclarationProvider = m.Opt.DeclarationProvider
	//} else if m.onDeclaration != nil {
	//	resp.Capabilities.DeclarationProvider = true
	//}
	//if m.Opt.DefinitionProvider != nil {
	//	resp.Capabilities.DefinitionProvider = m.Opt.DefinitionProvider
	//} else if m.onDefinition != nil {
	//	resp.Capabilities.DefinitionProvider = true
	//}
	//if m.Opt.TypeDefinitionProvider != nil {
	//	resp.Capabilities.TypeDefinitionProvider = m.Opt.TypeDefinitionProvider
	//} else if m.onTypeDefinition != nil {
	//	resp.Capabilities.TypeDefinitionProvider = true
	//}
	//if m.Opt.ImplementationProvider != nil {
	//	resp.Capabilities.ImplementationProvider = m.Opt.ImplementationProvider
	//} else if m.onImplementation != nil {
	//	resp.Capabilities.ImplementationProvider = true
	//}

	//if m.Opt.ReferencesProvider != nil {
	//	resp.Capabilities.ReferencesProvider = m.Opt.ReferencesProvider
	//} else if m.onReferences != nil {
	//	resp.Capabilities.ReferencesProvider = true
	//}

	//if m.Opt.DocumentHighlightProvider != nil {
	//	resp.Capabilities.DocumentHighlightProvider = m.Opt.DocumentHighlightProvider
	//} else if m.onDocumentHighlight != nil {
	//	resp.Capabilities.DocumentHighlightProvider = true
	//}

	//if m.Opt.DocumentSymbolProvider != nil {
	//	resp.Capabilities.DocumentSymbolProvider = m.Opt.DocumentSymbolProvider
	//} else if m.onDocumentSymbolWithSliceDocumentSymbol != nil {
	//	resp.Capabilities.DocumentSymbolProvider = true
	//} else if m.onDocumentSymbolWithSliceSymbolInformation != nil {
	//	resp.Capabilities.DocumentSymbolProvider = true
	//}
	//if m.Opt.CodeActionProvider != nil {
	//	resp.Capabilities.CodeActionProvider = m.Opt.CodeActionProvider
	//} else if m.onCodeActionWithSliceCodeAction != nil {
	//	resp.Capabilities.CodeActionProvider = true
	//} else if m.onCodeActionWithSliceCommand != nil {
	//	resp.Capabilities.CodeActionProvider = true
	//}
	//if m.Opt.CodeLensProvider != nil {
	//	resp.Capabilities.CodeLensProvider = m.Opt.CodeLensProvider
	//} else if m.onCodeLens != nil {
	//	t := true
	//	resp.Capabilities.CodeLensProvider = &defines.CodeLensOptions{WorkDoneProgressOptions: defines.WorkDoneProgressOptions{WorkDoneProgress: &t}, ResolveProvider: &t}
	//}
	//if m.Opt.DocumentLinkProvider != nil {
	//	resp.Capabilities.DocumentLinkProvider = m.Opt.DocumentLinkProvider
	//} else if m.onDocumentLinks != nil {
	//	t := true
	//	resp.Capabilities.DocumentLinkProvider = &defines.DocumentLinkOptions{WorkDoneProgressOptions: defines.WorkDoneProgressOptions{WorkDoneProgress: &t}, ResolveProvider: &t}
	//}
	//if m.Opt.ColorProvider != nil {
	//	resp.Capabilities.ColorProvider = m.Opt.ColorProvider
	//} else if m.onDocumentColor != nil {
	//	resp.Capabilities.ColorProvider = true
	//}
	//if m.Opt.WorkspaceSymbolProvider != nil {
	//	resp.Capabilities.WorkspaceSymbolProvider = m.Opt.WorkspaceSymbolProvider
	//} else if m.onWorkspaceSymbol != nil {
	//	resp.Capabilities.WorkspaceSymbolProvider = true
	//}
	//if m.Opt.DocumentFormattingProvider != nil {
	//	resp.Capabilities.DocumentFormattingProvider = m.Opt.DocumentFormattingProvider
	//} else if m.onDocumentFormatting != nil {
	//	resp.Capabilities.DocumentFormattingProvider = true
	//}
	//if m.Opt.DocumentRangeFormattingProvider != nil {
	//	resp.Capabilities.DocumentRangeFormattingProvider = m.Opt.DocumentRangeFormattingProvider
	//} else if m.onDocumentRangeFormatting != nil {
	//	resp.Capabilities.DocumentRangeFormattingProvider = true
	//}
	//if m.Opt.DocumentOnTypeFormattingProvider != nil {
	//	resp.Capabilities.DocumentOnTypeFormattingProvider = m.Opt.DocumentOnTypeFormattingProvider
	//} else if m.onDocumentOnTypeFormatting != nil {
	//	// TODO
	//	resp.Capabilities.DocumentOnTypeFormattingProvider = &defines.DocumentOnTypeFormattingOptions{}
	//}
	//if m.Opt.RenameProvider != nil {
	//	resp.Capabilities.RenameProvider = m.Opt.RenameProvider
	//} else if m.onPrepareRename != nil {
	//	resp.Capabilities.RenameProvider = true
	//}
	//if m.Opt.FoldingRangeProvider != nil {
	//resp.Capabilities.FoldingRangeProvider = m.Opt.FoldingRangeProvider
	//} else if m.onFoldingRanges != nil {
	//resp.Capabilities.FoldingRangeProvider = true
	//}
	//if m.Opt.SelectionRangeProvider != nil {
	//resp.Capabilities.SelectionRangeProvider = m.Opt.SelectionRangeProvider
	//} else if m.onSelectionRanges != nil {
	//resp.Capabilities.SelectionRangeProvider = true
	//}
	//if m.Opt.ExecuteCommandProvider != nil {
	//resp.Capabilities.ExecuteCommandProvider = m.Opt.ExecuteCommandProvider
	//} else if m.onExecuteCommand != nil {
	//	// TODO
	//resp.Capabilities.ExecuteCommandProvider = &defines.ExecuteCommandOptions{}
	//}
	//if m.Opt.DocumentLinkProvider != nil {
	//resp.Capabilities.DocumentLinkProvider = m.Opt.DocumentLinkProvider
	//} else if m.onDocumentLinks != nil {
	//	// TODO
	//resp.Capabilities.DocumentLinkProvider = &defines.DocumentLinkOptions{}
	//}
	//if m.Opt.SemanticTokensProvider != nil {
	//resp.Capabilities.SemanticTokensProvider = m.Opt.SemanticTokensProvider
	//}

	//if m.Opt.MonikerProvider != nil {
	//resp.Capabilities.MonikerProvider = m.Opt.MonikerProvider
	//}

	//if m.Opt.CallHierarchyProvider != nil {
	//resp.Capabilities.CallHierarchyProvider = m.Opt.CallHierarchyProvider
	//}

	////}
	////if m.onMon != nil{
	////	resp.Capabilities.MonikerProvider = true
	////}
	////if m.onTypeHierarchy != nil{
	////	resp.Capabilities.TypeHierarchyProvider = true
	////}

	return resp
}
