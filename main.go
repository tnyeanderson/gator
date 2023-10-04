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
	jsonpatch "github.com/evanphx/json-patch"
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
}

func main() {
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Fatalf("error: failed to read stdin: %s", err.Error())
	}

	conf := &PluginConfig{}
	if err := json.Unmarshal(stdin, conf); err != nil {
		log.Fatalf("error: failed to parse JSON config: %s", err.Error())
	}

	tmpl, err := template.New("conf.Patch").Funcs(sprig.FuncMap()).Parse(conf.Patch)
	if err != nil {
		log.Fatalf("error: failed to parse JSON merge patch template: %s", err.Error())
	}

	type data interface{}
	var rawConf data
	err = json.Unmarshal(stdin, &rawConf)
	if err != nil {
		log.Fatalf("error: failed to parse JSON to plain interface: %s", err.Error())
	}

	merger := &bytes.Buffer{}
	if err = tmpl.Execute(merger, rawConf); err != nil {
		log.Fatalf("error: failed to execute template for JSON merge patch: %s", err.Error())
	}

	cleanup := fmt.Sprintf(`{"type": "%s", "plugin": null, "config": null, "patch": null}`, conf.Plugin)
	cleaned, err := jsonpatch.MergePatch(stdin, []byte(cleanup))
	if err != nil {
		log.Fatalf("error: failed to clean up undelegated config items: %s", err.Error())
	}

	downstreamConf := []byte("{}")
	if conf.Config != nil {
		downstreamConf = *conf.Config
	}
	downstream, err := jsonpatch.MergePatch(downstreamConf, merger.Bytes())
	if err != nil {
		log.Fatalf("error: failed to merge patch with downstream config: %s", err.Error())
	}

	finalConf, err := jsonpatch.MergePatch(cleaned, downstream)
	if err != nil {
		log.Fatalf("error: failed to merge downstream config with original: %s", err.Error())
	}

	// For debugging:
	//fmt.Println(string(finalConf))

	pluginPath, err := getPluginPath(conf.Plugin)
	if err != nil {
		log.Fatal(err.Error())
	}
	cmd := exec.Command(pluginPath)
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	cmd.Stdin = bytes.NewReader(finalConf)
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		log.Fatal(err.Error())
	}
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
