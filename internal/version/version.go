// Package version is the operator's release. release-please bumps it in the release PR
// (release-please-config.json, extra-files), so the image built from the tag reports it
// without a build argument.
package version

// Version is the operator's release.
const Version = "0.2.0" // x-release-please-version
