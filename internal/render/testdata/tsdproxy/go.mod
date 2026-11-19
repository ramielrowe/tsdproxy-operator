// This module is not part of the operator. It exists to pin the tsdproxy
// version that TestTSDProxyCompat checks the rendered configuration against.
//
// tsdproxy is not required from the operator's own go.mod on purpose: that
// would raise dozens of the operator's dependencies to the versions tsdproxy
// uses, for a dependency only one test needs.
//
// The go directive below follows tsdproxy's and cannot be lower. The
// operator's root go.mod must name at least the same Go version: CI installs
// the root's version, controller-gen ("make manifests") loads every go.mod in
// the repository including this one, and the test compiles tsdproxy itself.
module github.com/ramielrowe/tsdproxy-operator/internal/render/testdata/tsdproxy

go 1.26.7

// tsdproxy v3.0.0-beta.4. Its v2 and v3 tags are not valid Go module versions
// (the module path has no /v3 suffix), so Go addresses the tag's commit
// through a pseudo-version.
require github.com/almeidapaulopt/tsdproxy v1.4.4-0.20260930081404-aa26f04e2c47
