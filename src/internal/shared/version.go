package shared

// Version is the release both binaries report. Overridden at build time via
// -ldflags "-X cubpanel/internal/shared.Version=v0.1.17"; "dev" means the
// binary was built without a release stamp.
var Version = "dev"
