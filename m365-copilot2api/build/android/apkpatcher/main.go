package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `apkpatcher — Go replacement for the Android APK Python patchers

usage:
  apkpatcher identity <work> <oldPkg> <newPkg> <label> <versionCode> <versionName>
  apkpatcher smali <work>
  apkpatcher password <work> <secret-file>
  apkpatcher zipmode <unsigned.apk> <out.apk>
  apkpatcher verify <original.apk> <new.apk>
`)
}

func run(cmd string, args []string) error {
	switch cmd {
	case "identity":
		if len(args) != 6 {
			return fmt.Errorf("usage: apkpatcher identity <work> <oldPkg> <newPkg> <label> <versionCode> <versionName>")
		}
		return patchIdentity(args[0], args[1], args[2], args[3], args[4], args[5])
	case "smali":
		if len(args) != 1 {
			return fmt.Errorf("usage: apkpatcher smali <work>")
		}
		if err := patchFileChooser(args[0]); err != nil {
			return err
		}
		if err := patchHideNativeChrome(args[0]); err != nil {
			return err
		}
		if err := patchTurnstileCapture(args[0]); err != nil {
			return err
		}
		return patchFlareSolver(args[0])
	case "password":
		if len(args) != 2 {
			return fmt.Errorf("usage: apkpatcher password <work> <secret-file>")
		}
		return patchAdminPassword(args[0], args[1])
	case "zipmode":
		if len(args) != 2 {
			return fmt.Errorf("usage: apkpatcher zipmode <unsigned.apk> <out.apk>")
		}
		return restoreNativeMode(args[0], args[1])
	case "verify":
		if len(args) != 2 {
			return fmt.Errorf("usage: apkpatcher verify <original.apk> <new.apk>")
		}
		return verifyNativeReplacement(args[0], args[1])
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}
