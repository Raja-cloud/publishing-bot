/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	gogit "gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

// updateGomodWithTaggedDependencies gets the dependencies at the given tag and fills go.mod and go.sum.
// If anything is changed, it commits the changes. Returns true if go.mod changed.
func updateGomodWithTaggedDependencies(tag string, depsRepo []string) (bool, error) {
	found := map[string]bool{}
	changed := false

	for _, dep := range depsRepo {
		depPath := filepath.Join("..", dep)
		dr, err := gogit.PlainOpen(depPath)
		if err != nil {
			return changed, fmt.Errorf("failed to open dependency repo at %q: %v", depPath, err)
		}

		depPkg, err := fullPackageName(depPath)
		if err != nil {
			return changed, fmt.Errorf("failed to get package at %s: %v", depPath, err)
		}

		commit, commitTime, err := taggedCommitHashAndTime(dr, tag)
		if err != nil {
			return changed, fmt.Errorf("failed to get tag %s for %q: %v", tag, depPkg, err)
		}
		rev := commit.String()
		pseudoVersion := fmt.Sprintf("v0.0.0-%s-%s", commitTime.UTC().Format("20060102150405"), rev[:12])

		// in case the pseudoVersion has not changed, running go mod download will help
		// in avoiding packaging it up if the pseudoVersion has been published already
		downloadCommand := exec.Command("go", "mod", "download")
		downloadCommand.Env = append(os.Environ(), "GO111MODULE=on", "GOPOXY=file://${GOPATH}/pkg/mod/cache/download")
		downloadCommand.Stdout = os.Stdout
		downloadCommand.Stderr = os.Stderr
		if err := downloadCommand.Run(); err != nil {
			return changed, fmt.Errorf("error running go mod download for %s: %v", depPkg, err)
		}

		// check if we have the pseudoVersion published already. if we don't, package it up
		// and save to local mod download cache.
		if err := packageDepToGoModCache(depPath, depPkg, rev, pseudoVersion, "", commitTime); err != nil {
			return changed, fmt.Errorf("failed to package %s dependency: %v", depPkg, err)
		}

		if err := updateGoModAndGoSum(depPkg, pseudoVersion, ""); err != nil {
			return changed, err
		}

		found[dep] = true
		fmt.Printf("Bumping %s in go.mod to %s\n.", depPkg, rev)
		changed = true
	}

	for _, dep := range depsRepo {
		if !found[dep] {
			fmt.Printf("Warning: dependency %s not found in go.mod.\n", dep)
		}
	}
	return changed, nil
}

type ModuleInfo struct {
	Version string
	Name    string
	Short   string
	Time    string
}

func packageDepToGoModCache(depPath, depPkg, commit, pseudoVersionOrTag, majorVersion string, commitTime time.Time) error {
	cacheDir := fmt.Sprintf("%s/pkg/mod/cache/download/%s/@v", os.Getenv("GOPATH"), depPkg)
	if majorVersion != "" {
		cacheDir = fmt.Sprintf("%s/pkg/mod/cache/download/%s/v%s/@v", os.Getenv("GOPATH"), depPkg, majorVersion)
	}
	goModFile := fmt.Sprintf("%s/%s.mod", cacheDir, pseudoVersionOrTag)

	if _, err := os.Stat(goModFile); err == nil {
		fmt.Printf("%s for %s is already packaged up.\n", pseudoVersionOrTag, depPkg)
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Could not check if %s exists: %v", goModFile, err)
	}

	fmt.Printf("Packaging up %s for %s into go mod cache.\n", pseudoVersionOrTag, depPkg)

	// create the cache if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(goModFile), os.FileMode(755)); err != nil {
		return fmt.Errorf("unable to create %s directory: %v", cacheDir, err)
	}

	// checkout the dep repo to the commit at the tag
	checkoutCommand := exec.Command("git", "checkout", commit)
	checkoutCommand.Dir = fmt.Sprintf("%s/src/%s", os.Getenv("GOPATH"), depPkg)
	checkoutCommand.Stdout = os.Stdout
	checkoutCommand.Stderr = os.Stderr
	if err := checkoutCommand.Run(); err != nil {
		return fmt.Errorf("failed to checkout %s at %s: %v", depPkg, commit, err)
	}

	// copy go.mod to the cache dir
	if err := copyFile(fmt.Sprintf("%s/go.mod", depPath), goModFile); err != nil {
		return fmt.Errorf("unable to copy %s file to %s to gomod cache for %s: %v", fmt.Sprintf("%s/go.mod", depPath), goModFile, depPkg, err)
	}

	// create info file in the cache dir
	moduleInfo := ModuleInfo{
		Version: pseudoVersionOrTag,
		Name:    commit,
		Short:   commit[:12],
		Time:    commitTime.UTC().Format("2006-01-02T15:04:05Z"),
	}

	moduleFile, err := json.Marshal(moduleInfo)
	if err != nil {
		return fmt.Errorf("error marshaling .info file for %s: %v", depPkg, err)
	}
	if err := ioutil.WriteFile(fmt.Sprintf("%s/%s.info", cacheDir, pseudoVersionOrTag), moduleFile, 0644); err != nil {
		return fmt.Errorf("failed to write %s file for %s: %v", fmt.Sprintf("%s/%s.info", cacheDir, pseudoVersionOrTag), depPkg, err)
	}

	// create the zip file in the cache dir. This zip file has the same hash
	// as of the zip file that would have been created by go mod download.
	zipCmd := []string{"/gomod-zip", "--package-name", depPkg, "--pseudo-version", pseudoVersionOrTag}
	if majorVersion != "" {
		zipCmd = append(zipCmd, "--major-version", majorVersion)
	}
	zipCommand := exec.Command(zipCmd[0], zipCmd[1:]...)
	zipCommand.Stdout = os.Stdout
	zipCommand.Stderr = os.Stderr
	if err := zipCommand.Run(); err != nil {
		return fmt.Errorf("failed to run gomod-zip for %s at %s: %v", depPkg, pseudoVersionOrTag, err)
	}

	// append the pseudoVersion/tag to the list file in the cache dir
	listFile, err := os.OpenFile(fmt.Sprintf("%s/list", cacheDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("unable to open list file in %s: %v", cacheDir, err)
	}
	defer listFile.Close()

	if _, err := listFile.WriteString(fmt.Sprintf("%s\n", pseudoVersionOrTag)); err != nil {
		return fmt.Errorf("unable to write to list file in %s: %v", cacheDir, err)
	}

	return nil
}

func updateGoModAndGoSum(depPkg, pseudoVersionOrTag, majorVersion string) error {
	requireDirective := fmt.Sprintf("%s@%s", depPkg, pseudoVersionOrTag)
	if majorVersion != "" {
		requireDirective = fmt.Sprintf("%s/v%s@%s", depPkg, majorVersion, pseudoVersionOrTag)
	}

	requireCommand := exec.Command("go", "mod", "edit", "-fmt", "-require", requireDirective)
	requireCommand.Env = append(os.Environ(), "GO111MODULE=on")
	requireCommand.Stdout = os.Stdout
	requireCommand.Stderr = os.Stderr
	if err := requireCommand.Run(); err != nil {
		return fmt.Errorf("unable to pin %s in the require section of go.mod to %s: %v", depPkg, pseudoVersionOrTag, err)
	}

	replaceDirective := fmt.Sprintf("%s=%s@%s", depPkg, depPkg, pseudoVersionOrTag)
	if majorVersion != "" {
		replaceDirective = fmt.Sprintf("%s/v%s=%s/v%s@%s", depPkg, majorVersion, depPkg, majorVersion, pseudoVersionOrTag)
	}

	replaceCommand := exec.Command("go", "mod", "edit", "-fmt", "-replace", replaceDirective)
	replaceCommand.Env = append(os.Environ(), "GO111MODULE=on")
	replaceCommand.Stdout = os.Stdout
	replaceCommand.Stderr = os.Stderr
	if err := replaceCommand.Run(); err != nil {
		return fmt.Errorf("unable to pin %s in the replace section of go.mod to %s: %v", depPkg, pseudoVersionOrTag, err)
	}

	if majorVersion != "" {
		dropReplaceCommmand := exec.Command("go", "mod", "edit", "-fmt", "-dropreplace", depPkg)
		dropReplaceCommmand.Env = append(os.Environ(), "GO111MODULE=on")
		dropReplaceCommmand.Stdout = os.Stdout
		dropReplaceCommmand.Stderr = os.Stderr
		if err := dropReplaceCommmand.Run(); err != nil {
			return fmt.Errorf("unable to drop %s from the replace section of go.mod: %v", depPkg, err)
		}
	}

	downloadCommand := exec.Command("go", "mod", "download")
	downloadCommand.Env = append(os.Environ(), "GO111MODULE=on", "GOPOXY=file://${GOPATH}/pkg/mod/cache/download")
	downloadCommand.Stdout = os.Stdout
	downloadCommand.Stderr = os.Stderr
	if err := downloadCommand.Run(); err != nil {
		return fmt.Errorf("error running go mod download for %s for %s: %v", pseudoVersionOrTag, depPkg, err)
	}

	tidyCommand := exec.Command("go", "mod", "tidy")
	tidyCommand.Env = append(os.Environ(), "GO111MODULE=on", "GOPOXY=file://${GOPATH}/pkg/mod/cache/download")
	tidyCommand.Stdout = os.Stdout
	tidyCommand.Stderr = os.Stderr
	if err := tidyCommand.Run(); err != nil {
		return fmt.Errorf("unable to run go mod tidy for %s at %s: %v", depPkg, pseudoVersionOrTag, err)
	}

	return nil
}

func taggedCommitHashAndTime(r *gogit.Repository, tag string) (plumbing.Hash, time.Time, error) {
	ref, err := r.Reference(plumbing.ReferenceName(fmt.Sprintf("refs/tags/%s", tag)), true)
	if err != nil {
		return plumbing.ZeroHash, time.Time{}, fmt.Errorf("failed to get refs/tags/%s: %v", tag, err)
	}

	tagObject, err := r.TagObject(ref.Hash())
	if err != nil {
		if err != nil {
			return plumbing.ZeroHash, time.Time{}, fmt.Errorf("refs/tags/%s is invalid: %v", tag, err)
		}
	}
	commitAtTag, err := tagObject.Commit()
	if err != nil {
		return plumbing.ZeroHash, time.Time{}, fmt.Errorf("failed to get underlying commit for tag %s: %v", tag, err)
	}
	return commitAtTag.Hash, commitAtTag.Committer.When, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("unable to open %s: %v", src, err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("unable to create %s: %v", dst, err)
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return fmt.Errorf("unable to copy %s to %s: %v", src, dst, err)
	}
	return out.Close()
}
