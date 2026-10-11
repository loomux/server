// Command manifeststamp writes a release into plugin manifests at build
// time (LOOM-194; docs/release/versioning.md, "Images"):
//
//	go run ./plugins/manifeststamp -version 0.3.80 \
//	    -agent-image ghcr.io/loomux/agent:0.3.77@sha256:... \
//	    plugins/kubernetes/plugin.json plugins/docker/plugin.json
//
// sets each manifest's version to the release and its min_host_version
// to the release's X.Y.0 (a plugin runs with the server of its own
// 0.Y line; a pre-release's minimum is itself), and the default of an
// agent_image setting to the image the release pins. The Dockerfiles
// run it before `go build`, so the manifest the binary embeds and the
// plugin.json shipped beside it are the same stamped file. In the
// tree the manifests say 0.0.0-dev; an empty version leaves them so.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var release = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// stamp returns the manifest data with version (when given; X.Y.Z or
// X.Y.Z-pre) and the agent image's default (when given, and the
// manifest has that setting) written in.
func stamp(data []byte, version, agentImage string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if _, ok := m["version"].(string); !ok {
		return nil, errors.New("manifest has no version")
	}
	if version != "" {
		if err := checkVersion(version); err != nil {
			return nil, err
		}
		m["version"] = version
		m["min_host_version"] = minHost(version)
	}
	if agentImage != "" {
		if schema, ok := m["config_schema"].(map[string]any); ok {
			if props, ok := schema["properties"].(map[string]any); ok {
				if prop, ok := props["agent_image"].(map[string]any); ok {
					prop["default"] = agentImage
				}
			}
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func checkVersion(v string) error {
	if !release.MatchString(v) {
		return fmt.Errorf("version %q isn't X.Y.Z (or X.Y.Z-pre)", v)
	}
	return nil
}

// minHost is the minimum a manifest stamped with version declares:
// X.Y.0, or the version itself when it is a pre-release.
func minHost(version string) string {
	if strings.Contains(version, "-") {
		return version
	}
	parts := strings.SplitN(version, ".", 3)
	return parts[0] + "." + parts[1] + ".0"
}

// run stamps the files named on the command line in place.
func run(args []string) error {
	fs := flag.NewFlagSet("manifeststamp", flag.ContinueOnError)
	version := fs.String("version", "", "the release (X.Y.Z); empty leaves the version alone")
	agentImage := fs.String("agent-image", "", "the agent image to make the default (name:tag@sha256:...); empty leaves it alone")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("no manifest named")
	}
	if *version != "" {
		if err := checkVersion(*version); err != nil {
			return err
		}
	}
	for _, path := range fs.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out, err := stamp(data, *version, *agentImage)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "manifeststamp:", err)
		os.Exit(1)
	}
}
