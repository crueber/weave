// Package version holds the build version, stamped with
// -ldflags "-X github.com/crueber/weave/internal/version.Version=vX.Y.Z".
// It is a leaf package so the slim CLI can use it without linking the server.
package version

// Version is the build version ("dev" for local builds).
var Version = "dev"
