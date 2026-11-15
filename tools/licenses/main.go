// Command licenses writes THIRD_PARTY_LICENSES.txt: the license text of
// every Go module compiled into rocket and rocket-tray for the release
// targets, plus the bundled web assets. It also writes the Android app's
// copy (android/app/src/main/assets/licenses/THIRD_PARTY_LICENSES.txt, shown
// in Settings > About): the Go toolchain's own license, the modules linked
// into mobile/rocketcore for Android, the gomobile runtime, the fonts and
// icons, and the AndroidX/Kotlin libraries. Run it from the repository root
// after changing dependencies (in go.mod, tray/go.mod or
// android/gradle/libs.versions.toml) or vendored assets:
//
//	go run ./tools/licenses            # rewrite both files
//	go run ./tools/licenses -check     # exit 1 if either file is out of date
//
// It is a development tool; rocket itself never runs it.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// binary is one program shipped in the release zips (scripts/build.*).
type binary struct {
	name    string   // shown in the file
	dir     string   // module directory, relative to the repository root
	pkg     string   // package path for go list, relative to dir
	targets []string // GOOS/GOARCH pairs it is released for
}

var binaries = []binary{
	{"rocket", ".", "./cmd/rocket", []string{
		"windows/amd64", "windows/arm64", "darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64",
	}},
	{"rocket-tray", "tray", ".", []string{
		"windows/amd64", "windows/arm64", "linux/amd64", "linux/arm64",
	}},
}

// assets are the third-party files embedded through web/.
var assets = []struct{ name, license string }{
	{"Alpine.js 3.14.9 (web/vendor/alpine.min.js)", "web/vendor/LICENSE-alpine.txt"},
	{"Inter Variable font (web/vendor/fonts/Inter-Variable-latin.woff2)", "web/vendor/fonts/OFL-Inter.txt"},
	{"JetBrains Mono fonts (web/vendor/fonts/JetBrainsMono-latin-*.woff2)", "web/vendor/fonts/OFL-JetBrainsMono.txt"},
	{"Lucide icons (web/icons.svg)", "web/vendor/LICENSE-lucide.txt"},
}

const out = "THIRD_PARTY_LICENSES.txt"

// androidOut is the Android app's license file, an asset of the APK.
const androidOut = "android/app/src/main/assets/licenses/THIRD_PARTY_LICENSES.txt"

// android is the Go side of the Android app: gomobile bind builds it for
// these ABIs (scripts/build-android.*), with cgo.
var android = binary{"rocket-android", ".", "./mobile/rocketcore", []string{
	"android/arm64", "android/arm", "android/amd64",
}}

// androidExtraModules are linked into the APK's Go library without being
// imported by mobile/rocketcore: the bind runtime gomobile generates code for.
var androidExtraModules = []string{"golang.org/x/mobile"}

// androidAssets are the app's bundled fonts and icons (copies of the web
// ones, see android/app/src/main/res/font) and its AndroidX, Compose and
// Kotlin libraries (android/gradle/libs.versions.toml), which are all under
// the Apache License 2.0, the same text as rocket's own LICENSE.
var androidAssets = []struct{ name, license string }{
	{"Inter Variable font (res/font/inter_variable.ttf)", "web/vendor/fonts/OFL-Inter.txt"},
	{"JetBrains Mono fonts (res/font/jetbrains_mono_*.ttf)", "web/vendor/fonts/OFL-JetBrainsMono.txt"},
	{"Lucide icons (ui/Icons.kt)", "web/vendor/LICENSE-lucide.txt"},
	{"AndroidX (core, activity, lifecycle), Jetpack Compose (ui, foundation, material3), " +
		"Kotlin standard library and kotlinx.coroutines: Apache License 2.0", "LICENSE"},
}

type module struct {
	Path, Version, Dir string
	Main               bool
}

func main() {
	check := flag.Bool("check", false, "exit 1 if "+out+" is out of date instead of writing it")
	flag.Parse()
	files := []struct {
		path string
		gen  func() ([]byte, error)
	}{{out, generate}, {androidOut, generateAndroid}}
	stale := false
	for _, f := range files {
		text, err := f.gen()
		if err != nil {
			fmt.Fprintln(os.Stderr, "licenses:", err)
			os.Exit(1)
		}
		if *check {
			old, _ := os.ReadFile(f.path)
			if !bytes.Equal(normalize(old), normalize(text)) {
				fmt.Fprintln(os.Stderr, f.path+" is out of date: run go run ./tools/licenses")
				stale = true
			}
			continue
		}
		if err := os.WriteFile(f.path, text, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "licenses:", err)
			os.Exit(1)
		}
	}
	if stale {
		os.Exit(1)
	}
}

// normalize ignores line-ending differences from git checkouts.
func normalize(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }

func generate() ([]byte, error) {
	mods := map[string]module{}
	usedBy := map[string]map[string]bool{}
	for _, bin := range binaries {
		for _, t := range bin.targets {
			goos, goarch, _ := strings.Cut(t, "/")
			list, err := deps(bin, goos, goarch)
			if err != nil {
				return nil, err
			}
			for _, m := range list {
				k := m.Path + "@" + m.Version
				mods[k] = m
				if usedBy[k] == nil {
					usedBy[k] = map[string]bool{}
				}
				usedBy[k][bin.name] = true
			}
		}
	}
	keys := make([]string, 0, len(mods))
	for k := range mods {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	b.WriteString("Third-party licenses for rocket\n")
	b.WriteString("===============================\n\n")
	b.WriteString("rocket is licensed under the Apache License, Version 2.0 (see LICENSE).\n")
	b.WriteString("The rocket and rocket-tray binaries include the following third-party\n")
	b.WriteString("components. This file is generated by tools/licenses; do not edit it by hand.\n\n")
	b.WriteString("Bundled web assets (rocket)\n---------------------------\n")
	for _, a := range assets {
		b.WriteString("  " + a.name + "\n")
	}
	b.WriteString("\nGo modules\n----------\n")
	for _, k := range keys {
		var users []string
		for _, bin := range binaries {
			if usedBy[k][bin.name] {
				users = append(users, bin.name)
			}
		}
		b.WriteString("  " + k + " (" + strings.Join(users, ", ") + ")\n")
	}

	for _, a := range assets {
		text, err := os.ReadFile(a.license)
		if err != nil {
			return nil, err
		}
		section(&b, a.name, text)
	}
	for _, k := range keys {
		text, err := moduleLicenses(mods[k].Dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		section(&b, k, text)
	}
	return b.Bytes(), nil
}

// generateAndroid writes the Android app's license file.
func generateAndroid() ([]byte, error) {
	mods := map[string]module{}
	for _, t := range android.targets {
		goos, goarch, _ := strings.Cut(t, "/")
		list, err := deps(android, goos, goarch)
		if err != nil {
			return nil, err
		}
		for _, m := range list {
			mods[m.Path+"@"+m.Version] = m
		}
	}
	for _, p := range androidExtraModules {
		m, err := moduleInfo(p)
		if err != nil {
			return nil, err
		}
		mods[m.Path+"@"+m.Version] = m
	}
	keys := make([]string, 0, len(mods))
	for k := range mods {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	b.WriteString("Third-party licenses for rocket for Android\n")
	b.WriteString("===========================================\n\n")
	b.WriteString("rocket is licensed under the Apache License, Version 2.0.\n")
	b.WriteString("The rocket Android app includes the following third-party components.\n")
	b.WriteString("This file is generated by tools/licenses; do not edit it by hand.\n\n")
	b.WriteString("Bundled assets and libraries\n----------------------------\n")
	for _, a := range androidAssets {
		b.WriteString("  " + a.name + "\n")
	}
	b.WriteString("\nGo\n--\n  The Go standard library and runtime (golang.org)\n")
	for _, k := range keys {
		b.WriteString("  " + k + "\n")
	}
	for _, a := range androidAssets {
		text, err := os.ReadFile(a.license)
		if err != nil {
			return nil, err
		}
		section(&b, a.name, text)
	}
	goLicense, err := goRootLicense()
	if err != nil {
		return nil, err
	}
	section(&b, "The Go standard library and runtime (golang.org)", goLicense)
	for _, k := range keys {
		text, err := moduleLicenses(mods[k].Dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		section(&b, k, text)
	}
	return b.Bytes(), nil
}

// goRootLicense is the Go distribution's LICENSE (the standard library and
// runtime are linked into every Go binary).
func goRootLicense() ([]byte, error) {
	root, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOROOT: %w", err)
	}
	return os.ReadFile(filepath.Join(strings.TrimSpace(string(root)), "LICENSE"))
}

// moduleInfo looks up one module of the main module's build list,
// downloading it when it is not in the module cache yet.
func moduleInfo(path string) (module, error) {
	cmd := exec.Command("go", "mod", "download", "-json", path)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Stderr = os.Stderr
	outb, err := cmd.Output()
	if err != nil {
		return module{}, fmt.Errorf("go mod download %s: %w", path, err)
	}
	var m module
	if err := json.Unmarshal(outb, &m); err != nil {
		return module{}, err
	}
	if m.Dir == "" {
		return module{}, fmt.Errorf("go mod download %s gave no folder", path)
	}
	return m, nil
}

func section(b *bytes.Buffer, name string, text []byte) {
	b.WriteString("\n\n" + strings.Repeat("=", 78) + "\n" + name + "\n" + strings.Repeat("=", 78) + "\n\n")
	b.Write(bytes.TrimSpace(normalize(text)))
	b.WriteString("\n")
}

// deps lists the non-main modules linked into one binary for one target.
func deps(bin binary, goos, goarch string) ([]module, error) {
	cmd := exec.Command("go", "list", "-deps", "-json=Module", bin.pkg)
	cmd.Dir = bin.dir
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0", "GOWORK=off")
	cmd.Stderr = os.Stderr
	outb, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s for %s/%s: %w", bin.name, goos, goarch, err)
	}
	var list []module
	dec := json.NewDecoder(bytes.NewReader(outb))
	for {
		var p struct{ Module *module }
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if p.Module != nil && !p.Module.Main {
			list = append(list, *p.Module)
		}
	}
	return list, nil
}

// moduleLicenses returns the LICENSE, COPYING and NOTICE files at the root
// of a module, in name order.
func moduleLicenses(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for _, e := range entries {
		n := strings.ToUpper(e.Name())
		if e.IsDir() || !(strings.HasPrefix(n, "LICENSE") || strings.HasPrefix(n, "LICENCE") ||
			strings.HasPrefix(n, "COPYING") || strings.HasPrefix(n, "NOTICE")) {
			continue
		}
		text, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		if b.Len() > 0 {
			b.WriteString("\n\n--- " + e.Name() + " ---\n\n")
		}
		b.Write(text)
	}
	if b.Len() == 0 {
		return nil, fmt.Errorf("no license file in %s", dir)
	}
	return b.Bytes(), nil
}
