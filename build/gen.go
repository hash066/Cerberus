package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	root, err := repoRoot()
	check(err)

	toolDirs := candidateToolDirs()
	env := prependPath(os.Environ(), toolDirs)

	buf := mustFind("buf", toolDirs)
	protocGenGo := mustFind("protoc-gen-go", toolDirs)
	witBindgen := mustFind("wit-bindgen", toolDirs)

	fmt.Printf("using buf: %s\n", buf)
	fmt.Printf("using protoc-gen-go: %s\n", protocGenGo)
	fmt.Printf("using wit-bindgen: %s\n", witBindgen)

	clean(root, filepath.Join("contract", "go", "gen"))
	clean(root, filepath.Join("components", "bindings", "rust"))

	run(filepath.Join(root, "proto"), env, buf, "generate")
	run(root, env, witBindgen,
		"rust",
		"--generate-all",
		"--world", "agent",
		"--out-dir", filepath.Join("components", "bindings", "rust"),
		filepath.Join("components", "wit"),
	)
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if exists(filepath.Join(dir, "Taskfile.yml")) && exists(filepath.Join(dir, "proto", "buf.yaml")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find repository root from %s", dir)
		}
		dir = parent
	}
}

func candidateToolDirs() []string {
	dirs := []string{}
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		dirs = append(dirs, filepath.Join(gopath, "bin"))
	} else if home := homeDir(); home != "" {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	if home := homeDir(); home != "" {
		dirs = append(dirs, filepath.Join(home, ".cargo", "bin"))
	}
	return dirs
}

func homeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	return os.Getenv("USERPROFILE")
}

func prependPath(env []string, dirs []string) []string {
	current := os.Getenv("PATH")
	prefix := strings.Join(uniqueExistingDirs(dirs), string(os.PathListSeparator))
	if prefix != "" {
		current = prefix + string(os.PathListSeparator) + current
	}

	updated := make([]string, 0, len(env)+1)
	for _, item := range env {
		if strings.HasPrefix(strings.ToUpper(item), "PATH=") {
			continue
		}
		updated = append(updated, item)
	}
	return append(updated, "PATH="+current)
}

func uniqueExistingDirs(dirs []string) []string {
	seen := map[string]bool{}
	unique := []string{}
	for _, dir := range dirs {
		if dir == "" || !exists(dir) {
			continue
		}
		key := strings.ToLower(filepath.Clean(dir))
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, dir)
	}
	return unique
}

func mustFind(name string, dirs []string) string {
	for _, dir := range uniqueExistingDirs(dirs) {
		for _, candidate := range executableNames(name) {
			path := filepath.Join(dir, candidate)
			if exists(path) {
				return path
			}
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	fatalf("%s not found; install it with the commands in docs/agent-briefs.md Brief D", name)
	return ""
}

func executableNames(name string) []string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return []string{name + ".exe", name}
	}
	return []string{name}
}

func clean(root, rel string) {
	target := filepath.Clean(filepath.Join(root, rel))
	cleanRoot := filepath.Clean(root)
	if target == cleanRoot || !strings.HasPrefix(target, cleanRoot+string(os.PathSeparator)) {
		fatalf("refusing to clean path outside repository: %s", target)
	}
	check(os.RemoveAll(target))
	check(os.MkdirAll(target, 0o755))
}

func run(dir string, env []string, name string, args ...string) {
	fmt.Printf("running: %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	check(cmd.Run())
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func check(err error) {
	if err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
