// loomux-plugin-docker is the Docker target-provider plugin's
// executable: stdio with no arguments (the bundled subprocess form),
// --listen <path> for a socket (the sidecar form).
package main

import (
	"github.com/Loomux/server/plugins/sdk"

	"github.com/Loomux/server/plugins/docker"
)

func main() {
	sdk.Main(docker.New())
}
