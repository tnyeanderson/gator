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
merged with stdin (gator's plugin configuration will be removed) and the
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

	sprig "github.com/Masterminds/sprig/v3"
	"github.com/containernetworking/cni/pkg/types"
	jsonpatch "github.com/evanphx/json-patch"
)

var (
	version = "dev"
	commit  = "HEAD"
)

const (
	ErrInvalidPatchTemplate = 100
	ErrMergeJSONFailed      = 101
	ErrDownstreamExecFailed = 102
)

// PluginConfig is the configuration for the gator plugin itself, provided via
// stdin.
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
}

func main() {
	// Handle --version flag
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("CNI gator plugin version %s commit %s\n", version, commit)
		os.Exit(0)
	}

	// Since jsonpatch requires []byte, we can't just pass io.Reader arguments
	// around. Therefore we are forced to buffer the input.
	stdin, ioerr := io.ReadAll(os.Stdin)
	if ioerr != nil {
		exit(types.NewError(types.ErrIOFailure, "failed to read stdin", ioerr.Error()))
		return
	}

	// Unmarshal config
	conf := &PluginConfig{}
	if err := json.Unmarshal(stdin, conf); err != nil {
		exit(types.NewError(types.ErrDecodingFailure, "failed to parse JSON config", err.Error()))
		return
	}

	// Skip delegation for defined CNI_COMMAND values
	if slices.Contains(conf.Skip, os.Getenv("CNI_COMMAND")) {
		fmt.Print(string(stdin))
		os.Exit(0)
	}

	// Get path of delegate plugin
	pluginPath, err := getPluginPath(conf.Plugin)
	if err != nil {
		exit(err)
		return
	}

	// Generate downstream config
	downstreamConfig, err := generateDownstream(stdin, conf)
	if err != nil {
		exit(err)
		return
	}

	// Run the delegated plugin
	cmd := exec.Command(pluginPath)
	cmd.Env = os.Environ()
	cmd.Stdin = bytes.NewReader(downstreamConfig)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			os.Exit(exiterr.ExitCode())
		}
		msg := fmt.Sprintf("exec error when calling downstream plugin: %s", pluginPath)
		exit(types.NewError(ErrDownstreamExecFailed, msg, err.Error()))
	}
}

// exit prints the CNI error to stderr and exits.
func exit(err *types.Error) {
	fmt.Fprint(os.Stderr, err.Error())
	os.Exit(int(err.Code))
}

// generateDownstream templates the patch using the data from stdin, executes
// the patch on the downstream config, removes gator's configuration items, and
// returns the resulting downstream config for delegation.
func generateDownstream(stdin []byte, conf *PluginConfig) ([]byte, *types.Error) {
	// Parse the patch as a template
	tmpl, err := template.New("conf.Patch").Funcs(sprig.FuncMap()).Parse(conf.Patch)
	if err != nil {
		return nil, types.NewError(
			types.ErrDecodingFailure,
			"failed to parse JSON merge patch template",
			err.Error(),
		)
	}

	// Gather all data from stdin for use in template
	var rawConf any
	err = json.Unmarshal(stdin, &rawConf)
	if err != nil {
		return nil, types.NewError(
			types.ErrDecodingFailure,
			"failed to parse stdin to plain interface",
			err.Error(),
		)
	}

	// Execute the template on the patch based on data from stdin
	merger := &bytes.Buffer{}
	if err = tmpl.Execute(merger, rawConf); err != nil {
		return nil, types.NewError(
			ErrInvalidPatchTemplate,
			"failed to execute template for JSON merge patch",
			err.Error(),
		)
	}

	// Remove gator's own configuration
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

	// Execute the patch on the downstream config
	downstream, err := jsonpatch.MergePatch(downstreamConf, patch)
	if err != nil {
		return nil, types.NewError(
			ErrMergeJSONFailed,
			"failed to merge patch with downstream config",
			err.Error(),
		)
	}

	// Merge the downstream config into the top-level config
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

// getPluginPath searches the CNI_PATH locations for the delegate plugin.
func getPluginPath(plugin string) (string, *types.Error) {
	cniPaths := filepath.SplitList(os.Getenv("CNI_PATH"))

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
