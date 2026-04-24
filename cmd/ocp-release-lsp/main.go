package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strings"

	"github.com/owenrumney/go-lsp/lsp"
	"github.com/owenrumney/go-lsp/server"
)

func rescan(registryPath string, comps []string, filename string) string {
	for joinLength := 2; joinLength < len(comps); joinLength++ {
		for joinPosition := 0; joinPosition <= len(comps)-joinLength; joinPosition++ {
			newComps := []string{}
			for k := 0; k < joinPosition; k++ {
				newComps = append(newComps, comps[k])
			}

			newComps = append(newComps, strings.Join(comps[joinPosition:joinPosition+joinLength], "-"))

			for k := joinPosition + joinLength; k < len(comps); k++ {
				newComps = append(newComps, comps[k])
			}

			directory := path.Join(append([]string{registryPath}, newComps...)...)
			fullPath := path.Join(directory, filename)

			if _, err := os.Stat(fullPath); err == nil {
				return fullPath
			}
		}
	}

	return ""
}

type handler struct {
	registryPath string
}

func (h *handler) Initialize(_ context.Context, params *lsp.InitializeParams) (*lsp.InitializeResult, error) {
	if len(params.WorkspaceFolders) == 0 {
		return nil, fmt.Errorf("no workspace folders")
	}

	folder := strings.TrimPrefix(string(params.WorkspaceFolders[0].URI), "file://")

	if _, err := os.Stat(folder); err != nil {
		return nil, fmt.Errorf("workspace directory error: %w", err)
	}

	h.registryPath = path.Join(folder, "ci-operator", "step-registry")

	return &lsp.InitializeResult{
		ServerInfo: &lsp.ServerInfo{Name: "ocp-release-lsp", Version: "0.1.0"},
	}, nil
}

func (h *handler) Shutdown(_ context.Context) error {
	return nil
}

func (h *handler) Definition(_ context.Context, params *lsp.DefinitionParams) ([]lsp.Location, error) {
	filePath := strings.TrimPrefix(string(params.TextDocument.URI), "file://")
	yamlFile, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	lines := strings.Split(string(yamlFile), "\n")
	if int(params.Position.Line) >= len(lines) {
		return nil, nil
	}
	line := lines[params.Position.Line]

	keyVal := strings.Split(line, ":")
	if len(keyVal) < 2 {
		return nil, nil
	}
	key := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(keyVal[0]), "-"))
	val := strings.TrimSpace(keyVal[1])

	comps := strings.Split(val, "-")

	var extension string
	switch key {
	case "workflow", "chain", "ref":
		extension = ".yaml"
	case "commands":
		comps = comps[:len(comps)-1]
		extension = ".sh"
	default:
		return nil, nil
	}

	directory := path.Join(append([]string{h.registryPath}, comps...)...)
	filename := strings.Join(append(comps, key), "-") + extension
	fullPath := path.Join(directory, filename)

	if _, err := os.Stat(fullPath); errors.Is(err, os.ErrNotExist) {
		fullPath = rescan(h.registryPath, comps, filename)
	}

	if fullPath == "" {
		return nil, nil
	}

	return []lsp.Location{{
		URI: lsp.DocumentURI("file://" + fullPath),
	}}, nil
}

func (h *handler) Hover(_ context.Context, _ *lsp.HoverParams) (*lsp.Hover, error) {
	return &lsp.Hover{
		Contents: lsp.MarkupContent{Kind: lsp.PlainText, Value: "hello world"},
	}, nil
}

func main() {
	srv := server.NewServer(&handler{})
	if err := srv.Run(context.Background(), server.RunStdio()); err != nil {
		log.Fatal(err)
	}
}
