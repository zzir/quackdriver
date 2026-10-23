package quackdriver

import (
	"fmt"
	"runtime"
)

// Version is the quackdriver release. It is reported to the server as part
// of the client platform string.
const Version = "0.2.0"

// clientDuckDBVersion is sent as client_duckdb_version in the handshake. The
// driver speaks the Quack protocol of this DuckDB release.
const clientDuckDBVersion = "v2.0.0"

// clientPlatform is sent as client_platform in the handshake, e.g.
// "quackdriver/0.2.0 go1.26.0 linux_amd64".
func clientPlatform() string {
	return fmt.Sprintf("quackdriver/%s %s %s_%s", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
