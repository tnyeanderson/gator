/*
gator is a templating delegator (get it?) CNI meta plugin. It allows a CNI
plugin's configuration to be dynamically generated at runtime based on the
result from previous plugins in the chain.

It takes the name of the downstream plugin, configuration for the downstream
plugin, and a JSON merge patch to be applied to that configuration.

The patch can include golang text/template syntax which will be executed based
on the full input from stdin before the patch is applied to the downstream
configuration.

Once the patch has been applied to the downstream configuration, it will be
merged with stdin (gator's plugin configuration will be removed) and the the
downstream plugin will be called with the same environment and the new,
templated, patched stdin... just as if it had been called originally, but now
you can dynamically configure plugins based on previous results!
*/
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	sprig "github.com/Masterminds/sprig/v3"
	"github.com/containernetworking/cni/pkg/types"
	jsonpatch "github.com/evanphx/json-patch"
)

const (
	Version                 = "v0.0.1"
	ErrInvalidPatchTemplate = 100
	ErrMergeJSONFailed      = 101
)

type PluginConfig struct {
	// Config is the configuration for the downstream CNI plugin.
	Config *json.RawMessage

	// Patch is a templatable RFC7396 JSON merge patch which will be applied to
	// Config. Before the patch is applied, a golang text/template based on the
	// incoming stdin data (as a plain interface) will be executed on it. This
	// means that you can use any value that is available via stdin as a template
	// value in the merge patch.
	Patch string

	// Plugin is the name of the downstream CNI plugin which will be called.
	Plugin string

	// Skip is an array of CNI_COMMAND values for which no action will be taken.
	Skip []string

	// stdin is the original stdin that gator received
	stdin []byte

	// downstreamConfig is what will be sent as stdin to the delegated plugin.
	downstreamConfig []byte
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("CNI gator plugin %s\n", Version)
		os.Exit(0)
	}

	stdin, ioerr := io.ReadAll(os.Stdin)
	if ioerr != nil {
		err := types.NewError(
			types.ErrIOFailure,
			"failed to read stdin",
			ioerr.Error(),
		)
		handleError(err)
		return
	}

	conf, err := parseConf(stdin)

	if err != nil {
		handleError(err)
	}

	// For debugging:
	//fmt.Println(string(conf.downstreamConfig))

	pluginPath, err := getPluginPath(conf.Plugin)
	if err != nil {
		handleError(err)
	}

	stdout, stderr, exitcode := delegate(pluginPath, conf.downstreamConfig, os.Environ())

	fmt.Print(string(stdout))
	fmt.Fprint(os.Stderr, string(stderr))
	os.Exit(exitcode)
}

func handleError(err *types.Error) {
	fmt.Fprint(os.Stderr, err.Error())
	os.Exit(int(err.Code))
}

// parseConf will return a complete [PluginConfig] based on stdin. If the
// [PluginConfig.Skip] contains the CNI_COMMAND, it will immediately print what
// it received on stdin and exit. If an error is encountered, it is returned as
// a [types.Error].
func parseConf(stdin []byte) (conf *PluginConfig, err *types.Error) {
	conf = &PluginConfig{stdin: stdin}
	if err := json.Unmarshal(stdin, conf); err != nil {
		return nil, types.NewError(
			types.ErrDecodingFailure,
			"failed to parse JSON config",
			err.Error(),
		)
	}

	if slices.Contains(conf.Skip, os.Getenv("CNI_COMMAND")) {
		fmt.Print(string(stdin))
		os.Exit(0)
	}

	downstreamConfig, err := generateDownstream(conf)
	if err != nil {
		return conf, err
	}

	conf.downstreamConfig = downstreamConfig
	return conf, nil
}

func generateDownstream(conf *PluginConfig) ([]byte, *types.Error) {
	stdin := conf.stdin
	tmpl, err := template.New("conf.Patch").Funcs(sprig.FuncMap()).Parse(conf.Patch)
	if err != nil {
		return nil, types.NewError(
			types.ErrDecodingFailure,
			"failed to parse JSON merge patch template",
			err.Error(),
		)
	}

	type data interface{}
	var rawConf data
	err = json.Unmarshal(stdin, &rawConf)
	if err != nil {
		return nil, types.NewError(
			types.ErrDecodingFailure,
			"failed to parse stdin to plain interface",
			err.Error(),
		)
	}

	merger := &bytes.Buffer{}
	if err = tmpl.Execute(merger, rawConf); err != nil {
		return nil, types.NewError(
			ErrInvalidPatchTemplate,
			"failed to execute template for JSON merge patch",
			err.Error(),
		)
	}

	cleanup := fmt.Sprintf(`{"type": "%s", "plugin": null, "config": null, "patch": null}`, conf.Plugin)
	cleaned, err := jsonpatch.MergePatch(stdin, []byte(cleanup))
	if err != nil {
		return nil, types.NewError(
			ErrMergeJSONFailed,
			"failed to clean up undelegated config items",
			err.Error(),
		)
	}

	// Allow no-op configs
	downstreamConf := []byte("{}")
	if conf.Config != nil {
		downstreamConf = *conf.Config
	}
	patch := merger.Bytes()
	if len(patch) == 0 {
		patch = []byte("{}")
	}

	downstream, err := jsonpatch.MergePatch(downstreamConf, patch)
	if err != nil {
		return nil, types.NewError(
			ErrMergeJSONFailed,
			"failed to merge patch with downstream config",
			err.Error(),
		)
	}

	finalConfig, err := jsonpatch.MergePatch(cleaned, downstream)
	if err != nil {
		return nil, types.NewError(
			ErrMergeJSONFailed,
			"failed to merge downstream config with original",
			err.Error(),
		)
	}

	return finalConfig, nil
}

func delegate(pluginPath string, stdin []byte, env []string) (stdout []byte, stderr []byte, exitcode int) {
	fout := &bytes.Buffer{}
	ferr := &bytes.Buffer{}

	cmd := exec.Command(pluginPath)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = fout
	cmd.Stderr = ferr

	if err := cmd.Run(); err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			exitcode = exiterr.ExitCode()
		}
	}

	return fout.Bytes(), ferr.Bytes(), exitcode
}

func getPluginPath(plugin string) (string, *types.Error) {
	cniPaths := []string{}
	if cniPathVar := os.Getenv("CNI_PATH"); cniPathVar != "" {
		cniPaths = append(cniPaths, strings.Split(cniPathVar, ":")...)
	} else {
		cniPaths = []string{"/opt/cni/bin"}
	}

	for _, p := range cniPaths {
		fullPath := filepath.Join(p, plugin)
		f, err := os.Open(fullPath)
		if err != nil {
			continue
		}
		s, err := f.Stat()
		if err != nil {
			continue
		}
		// Check if file is executable by someone
		if s.Mode()&0111 != 0 {
			return fullPath, nil
		}
	}
	return "", types.NewError(
		ErrMergeJSONFailed,
		fmt.Sprintf("cni executable not found in CNI_PATH: %s", plugin),
		fmt.Sprintf("checked: %v", cniPaths),
	)
}
