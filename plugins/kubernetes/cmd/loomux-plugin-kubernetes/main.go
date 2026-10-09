// loomux-plugin-kubernetes is the Kubernetes target-provider plugin's
// executable: stdio with no arguments (the bundled subprocess form),
// --listen <path> for a socket (the sidecar form).
package main

import (
	"github.com/Loomux/server/plugins/sdk"

	"github.com/Loomux/server/plugins/kubernetes"
)

func main() {
	sdk.Main(kubernetes.New())
}
