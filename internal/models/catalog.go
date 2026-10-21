package models

import (
	_ "embed"
	"encoding/json"
	"maps"
	"slices"
)

//go:embed catalog.json
var catalogJSON []byte

// Catalog lists the model bundles known to this binary.
type Catalog struct {
	Default string           `json:"default"`
	Bundles map[string]Entry `json:"bundles"`
}

// Entry describes a catalog bundle and its archive checksum.
type Entry struct {
	Variant string `json:"variant"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// EmbeddedCatalog returns the catalog built into the binary.
func EmbeddedCatalog() Catalog {
	var c Catalog
	if err := json.Unmarshal(catalogJSON, &c); err != nil {
		panic("models: catalog.json: " + err.Error()) // a build defect, which the package tests catch
	}
	return c
}

// IDs returns the bundle ids of the catalog, sorted.
func (c Catalog) IDs() []string {
	return slices.Sorted(maps.Keys(c.Bundles))
}
