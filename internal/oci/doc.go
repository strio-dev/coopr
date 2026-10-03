// Package oci resolves OCI image and component references to verified content.
//
// A component manifest's config blob contains the selected planner definition
// and one package entry per package layer. Each package entry carries both a
// snapshot descriptor and its complete OCI image configuration. A later
// from="package" stage inherits that configuration; copy from the package
// consumes only snapshot bytes. The enclosing config descriptor binds these
// package configuration bytes into the artifact digest.
package oci
