package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var identityComponents = []string{
	"MainActivity", "AuthActivity", "DiagActivity", "TunnelActivity",
	"WakeProvider", "GatewayService", "KeepAliveReceiver", "BootReceiver",
}

func patchIdentity(work, oldPkg, newPkg, label, versionCode, versionName string) error {
	root := filepath.Clean(work)
	manifestPath := filepath.Join(root, "AndroidManifest.xml")
	manifest, err := readFile(manifestPath)
	if err != nil {
		return err
	}
	switch {
	case strings.Contains(manifest, `package="`+oldPkg+`"`):
		manifest = strings.Replace(manifest, `package="`+oldPkg+`"`, `package="`+newPkg+`"`, 1)
	case strings.Contains(manifest, `package="`+newPkg+`"`):
		// already renamed
	default:
		return fmt.Errorf("expected package not found: %s or %s", oldPkg, newPkg)
	}
	manifest = strings.ReplaceAll(manifest, `"`+oldPkg+`.wake"`, `"`+newPkg+`.wake"`)
	for _, component := range identityComponents {
		manifest = strings.ReplaceAll(manifest, `android:name=".`+component+`"`, `android:name="`+oldPkg+`.`+component+`"`)
	}
	for _, action := range []string{"KEEPALIVE", "START", "STOP", "TUNNEL_START"} {
		manifest = strings.ReplaceAll(manifest, `"`+oldPkg+`.`+action+`"`, `"`+newPkg+`.`+action+`"`)
	}
	if err := writeFile(manifestPath, manifest); err != nil {
		return err
	}

	stringsPath := filepath.Join(root, "res", "values", "strings.xml")
	stringsXML, err := readFile(stringsPath)
	if err != nil {
		return err
	}
	nameRe := regexp.MustCompile(`<string name="app_name">[^<]*</string>`)
	replaced := nameRe.ReplaceAllString(stringsXML, `<string name="app_name">`+label+`</string>`)
	if replaced == stringsXML && !strings.Contains(stringsXML, `>`+label+`<`) {
		return fmt.Errorf("app_name string not found")
	}
	if err := writeFile(stringsPath, replaced); err != nil {
		return err
	}

	ymlPath := filepath.Join(root, "apktool.yml")
	yml, err := readFile(ymlPath)
	if err != nil {
		return err
	}
	yml = regexp.MustCompile(`versionCode: '[^']*'`).ReplaceAllString(yml, "versionCode: '"+versionCode+"'")
	yml = regexp.MustCompile(`versionName: .*`).ReplaceAllString(yml, "versionName: "+versionName)
	if err := writeFile(ymlPath, yml); err != nil {
		return err
	}

	smaliDirs, err := collectSmaliDirs(root)
	if err != nil {
		return err
	}
	replacements := [][2]string{
		{oldPkg + ".KEEPALIVE", newPkg + ".KEEPALIVE"},
		{oldPkg + ".START", newPkg + ".START"},
		{oldPkg + ".STOP", newPkg + ".STOP"},
		{oldPkg + ".TUNNEL_START", newPkg + ".TUNNEL_START"},
		{oldPkg + ".wake", newPkg + ".wake"},
	}
	for _, dir := range smaliDirs {
		if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".smali") {
				return err
			}
			text, err := readFile(path)
			if err != nil {
				return err
			}
			old := text
			for _, pair := range replacements {
				text = strings.ReplaceAll(text, pair[0], pair[1])
			}
			if text != old {
				return writeFile(path, text)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	for _, component := range identityComponents {
		suffix := filepath.Join("com", "m365", "gateway", component+".smali")
		found := false
		for _, dir := range smaliDirs {
			if _, err := os.Stat(filepath.Join(dir, suffix)); err == nil {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("component class missing: %s", suffix)
		}
	}
	manifestAfter, err := readFile(manifestPath)
	if err != nil {
		return err
	}
	rel := regexp.MustCompile(`android:name="\.(?:` + strings.Join(identityComponents, "|") + `)"`)
	if rel.MatchString(manifestAfter) {
		return fmt.Errorf("relative application component remains in manifest")
	}
	for _, token := range []string{oldPkg + ".wake", oldPkg + ".KEEPALIVE", oldPkg + ".START", oldPkg + ".STOP", oldPkg + ".TUNNEL_START"} {
		if strings.Contains(manifestAfter, token) {
			return fmt.Errorf("old private identity remains: %s in %s", token, manifestPath)
		}
		for _, dir := range smaliDirs {
			if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(path, ".smali") {
					return err
				}
				text, err := readFile(path)
				if err != nil {
					return err
				}
				if strings.Contains(text, token) {
					return fmt.Errorf("old private identity remains: %s in %s", token, path)
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	fmt.Printf("patched package identity: %s -> %s\n", oldPkg, newPkg)
	return nil
}

func collectSmaliDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "smali") {
			dirs = append(dirs, filepath.Join(root, entry.Name()))
		}
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no smali directories in %s", root)
	}
	return dirs, nil
}
