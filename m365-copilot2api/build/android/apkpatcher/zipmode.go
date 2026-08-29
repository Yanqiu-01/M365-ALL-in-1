package main

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strings"
)

func restoreNativeMode(source, target string) error {
	in, err := zip.OpenReader(source)
	if err != nil {
		return err
	}
	defer in.Close()
	outFile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer outFile.Close()
	out := zip.NewWriter(outFile)
	defer out.Close()
	for _, item := range in.File {
		header := item.FileHeader
		if strings.HasPrefix(item.Name, "lib/") && strings.HasSuffix(item.Name, ".so") {
			header.CreatorVersion = (header.CreatorVersion & 0xff) | (3 << 8)
			header.ExternalAttrs = 0o100700 << 16
		}
		writer, err := out.CreateHeader(&header)
		if err != nil {
			return err
		}
		reader, err := item.Open()
		if err != nil {
			return err
		}
		if _, err := io.Copy(writer, reader); err != nil {
			reader.Close()
			return err
		}
		reader.Close()
	}
	return nil
}

func verifyNativeReplacement(oldAPK, newAPK string) error {
	oldZ, err := zip.OpenReader(oldAPK)
	if err != nil {
		return err
	}
	defer oldZ.Close()
	newZ, err := zip.OpenReader(newAPK)
	if err != nil {
		return err
	}
	defer newZ.Close()
	so := "lib/arm64-v8a/libm365.so"
	cf := "lib/arm64-v8a/libcloudflared.so"
	oldSO, err := hashZip(oldZ, so)
	if err != nil {
		return err
	}
	newSO, err := hashZip(newZ, so)
	if err != nil {
		return err
	}
	oldCF, err := hashZip(oldZ, cf)
	if err != nil {
		return err
	}
	newCF, err := hashZip(newZ, cf)
	if err != nil {
		return err
	}
	fmt.Println("libm365.so 已替换 :", oldSO != newSO)
	fmt.Println("libcloudflared 未动:", oldCF == newCF)
	for _, name := range []string{so, cf} {
		info, err := zipInfo(newZ, name)
		if err != nil {
			return err
		}
		mode := (info.ExternalAttrs >> 16) & 0xffff
		fmt.Println(name, "mode=", fmt.Sprintf("%#o", mode), "compress=", info.Method)
	}
	fmt.Println("条目数            :", len(oldZ.File), "->", len(newZ.File))
	if oldSO == newSO {
		return fmt.Errorf("libm365.so was not replaced")
	}
	if oldCF != newCF {
		return fmt.Errorf("libcloudflared.so was modified")
	}
	return nil
}

func hashZip(z *zip.ReadCloser, name string) (string, error) {
	for _, item := range z.File {
		if item.Name != name {
			continue
		}
		reader, err := item.Open()
		if err != nil {
			return "", err
		}
		sum := sha256.New()
		if _, err := io.Copy(sum, reader); err != nil {
			reader.Close()
			return "", err
		}
		reader.Close()
		return fmt.Sprintf("%x", sum.Sum(nil)), nil
	}
	return "", fmt.Errorf("zip entry missing: %s", name)
}

func zipInfo(z *zip.ReadCloser, name string) (*zip.FileHeader, error) {
	for _, item := range z.File {
		if item.Name == name {
			header := item.FileHeader
			return &header, nil
		}
	}
	return nil, fmt.Errorf("zip entry missing: %s", name)
}
