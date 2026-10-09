// loomux-plugin-fake is the fake plugin's executable (LOOM-178): a
// loomux-plugin/1 plugin with nothing behind it, for the host's tests
// and the conformance suite. Not shipped in the image.
package main

import (
	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/sdk"
)

func main() {
	sdk.Main(fake.New())
}
