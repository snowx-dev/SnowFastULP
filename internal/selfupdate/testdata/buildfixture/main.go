// Command buildfixture is a minimal SnowFastULP binary used by the selfupdate
// tests as an "existing SnowFast binary" fixture: because it lives in this
// module tree, debug/buildinfo reports
// github.com/snowx-dev/SnowFastULP and the overwrite guard accepts it.
//
// The tests build it on demand (go build -o …); go test ignores this
// directory via the testdata convention.
package main

import "fmt"

// fixtureVersion is set at test-build time via -ldflags so each test run
// recompiles even when go's build cache would otherwise skip it.
var fixtureVersion = "dev"

func main() {
	fmt.Println("SnowFast", fixtureVersion)
}
