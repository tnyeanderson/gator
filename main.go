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
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	sprig "github.com/Masterminds/sprig/v3"
	"github.com/containernetworking/cni/pkg/types"
	jsonpatch "github.com/evanphx/json-patch"
)

const (
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

	// finalConfig is the config that will be provided to stdin when the
	// delegated plugin is called.
	finalConfig []byte
}

func main() {
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		err := types.NewError(
			types.ErrIOFailure,
			"failed to read stdin",
			err.Error(),
		)
		handleError(err)
		return
	}

	conf, err := prepare(stdin)
	if err != nil {
		handleError(err)
	}

	// For debugging:
	//fmt.Println(string(conf.finalConfig))

	if err := delegate(conf.Plugin, conf.finalConfig, os.Environ()); err != nil {
		handleError(err)
	}
}

func prepare(stdin []byte) (*PluginConfig, error) {
	conf := &PluginConfig{}
	if err := json.Unmarshal(stdin, conf); err != nil {
		return nil, types.NewError(
			types.ErrDecodingFailure,
			"failed to parse JSON config",
			err.Error(),
		)
	}

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

	conf.finalConfig = finalConfig
	return conf, nil
}

func handleError(err error) {
	log.Println(err)
}

func delegate(plugin string, stdin []byte, env []string) error {
	pluginPath, err := getPluginPath(plugin)
	if err != nil {
		return err
	}
	cmd := exec.Command(pluginPath)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func getPluginPath(plugin string) (string, error) {
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
	return "", fmt.Errorf("error: cni executable not found in CNI_PATH: %s", plugin)
}
