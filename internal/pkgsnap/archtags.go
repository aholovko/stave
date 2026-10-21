package pkgsnap

import (
	"errors"
	"fmt"
	"go/build"
	"os"
	"slices"
	"strings"
)

// These defaults mirror Go 1.26.5's internal/buildcfg, which cannot be imported.
// The host-architecture test detects drift from build.Default when Go is upgraded.
const (
	defaultGO386     = "sse2"
	defaultGOAMD64   = "v1"
	defaultGOARM     = "7"
	defaultGOARM64   = "v8.0"
	defaultGOMIPS    = "hardfloat"
	defaultGOMIPS64  = "hardfloat"
	defaultGOPPC64   = "power8"
	defaultGORISCV64 = "rva20u64"
)

// toolTags replaces host architecture tags when selecting another GOARCH.
// Changing build.Context.GOARCH does not recompute ToolTags, so keeping the host
// tags would select the wrong files. Non-architecture tags remain unchanged.
func toolTags(goarch string) ([]string, error) {
	host := build.Default.GOARCH
	if goarch == host {
		return build.Default.ToolTags, nil
	}
	features, err := archFeatureTags(goarch)
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, tag := range build.Default.ToolTags {
		if !strings.HasPrefix(tag, host+".") {
			tags = append(tags, tag)
		}
	}
	return append(tags, features...), nil
}

// archFeatureTags mirrors Go 1.26.5's internal/buildcfg for cross-architecture
// selection. Each feature level includes lower levels; float and extension suffixes
// affect code generation but add no build tags. Invalid settings return errors.
func archFeatureTags(goarch string) ([]string, error) {
	env := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	var features []string
	var err error
	switch goarch {
	case "386":
		// buildcfg passes GO386 through unchecked. The compiler rejects a bad value.
		features = []string{env("GO386", defaultGO386)}
	case "amd64":
		features, err = levelsUpTo("GOAMD64", env("GOAMD64", defaultGOAMD64), "v1", "v2", "v3", "v4")
	case "arm":
		v := env("GOARM", defaultGOARM)
		v = strings.TrimSuffix(v, ",softfloat")
		v = strings.TrimSuffix(v, ",hardfloat")
		if features, err = levelsUpTo("GOARM", v, "5", "6", "7"); err != nil {
			err = errors.New(`invalid GOARM: must start with 5, 6, or 7, and may optionally end in either ",hardfloat" or ",softfloat"`)
		}
	case "arm64":
		features, err = arm64Features(env("GOARM64", defaultGOARM64))
	case "mips", "mipsle":
		v := env("GOMIPS", defaultGOMIPS)
		if v != "hardfloat" && v != "softfloat" {
			return nil, errors.New("invalid GOMIPS: must be hardfloat, softfloat")
		}
		features = []string{v}
	case "mips64", "mips64le":
		v := env("GOMIPS64", defaultGOMIPS64)
		if v != "hardfloat" && v != "softfloat" {
			return nil, errors.New("invalid GOMIPS64: must be hardfloat, softfloat")
		}
		features = []string{v}
	case "ppc64", "ppc64le":
		features, err = levelsUpTo("GOPPC64", env("GOPPC64", defaultGOPPC64), "power8", "power9", "power10")
	case "riscv64":
		features, err = levelsUpTo("GORISCV64", env("GORISCV64", defaultGORISCV64), "rva20u64", "rva22u64", "rva23u64")
	case "wasm":
		// Both features are always enabled. GOWASM is only checked.
		for _, opt := range strings.Split(os.Getenv("GOWASM"), ",") {
			switch opt {
			case "", "satconv", "signext":
			default:
				return nil, fmt.Errorf("invalid GOWASM: no such feature %q", opt)
			}
		}
		features = []string{"satconv", "signext"}
	}
	if err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(features))
	for _, feature := range features {
		tags = append(tags, goarch+"."+feature)
	}
	return tags, nil
}

func levelsUpTo(name, value string, levels ...string) ([]string, error) {
	i := slices.Index(levels, value)
	if i < 0 {
		return nil, fmt.Errorf("invalid %s: must be %s", name, strings.Join(levels, ", "))
	}
	return levels[:i+1], nil
}

// arm64Features expands a GOARM64 value. v8.N activates v8.0 through v8.N. v9.N activates v9.0 through v9.N together
// with v8.0 through v8.(N+5), because v9.0 diverged from v8.5.
func arm64Features(v string) ([]string, error) {
	for {
		if s := strings.TrimSuffix(v, ",lse"); s != v {
			v = s
			continue
		}
		if s := strings.TrimSuffix(v, ",crypto"); s != v {
			v = s
			continue
		}
		break
	}
	switch v {
	case "v8.0", "v8.1", "v8.2", "v8.3", "v8.4", "v8.5", "v8.6", "v8.7", "v8.8", "v8.9",
		"v9.0", "v9.1", "v9.2", "v9.3", "v9.4", "v9.5":
	default:
		return nil, errors.New(`invalid GOARM64: must start with v8.{0-9} or v9.{0-5} and may optionally end in ",lse" and/or ",crypto"`)
	}
	major, minor := int(v[1]-'0'), int(v[3]-'0')
	var features []string
	for i := 0; i <= minor; i++ {
		features = append(features, fmt.Sprintf("v%d.%d", major, i))
	}
	if major == 9 {
		for i := 0; i <= minor+5 && i <= 9; i++ {
			features = append(features, fmt.Sprintf("v8.%d", i))
		}
	}
	return features, nil
}
