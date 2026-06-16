package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/bmjdotnet/teamster/internal/config"
)

// runInstallRemote execs into the install-remote.sh shell script,
// passing through all arguments. The Go code is just a launcher.
func runInstallRemote(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to load config: %v\n", err)
		return 1
	}

	basedir := filepath.Dir(cfg.DataDir) // DataDir = <basedir>/var
	scriptPath := filepath.Join(basedir, "lib", "scripts", "install-remote.sh")

	fi, err := os.Stat(scriptPath)
	if err != nil || fi.IsDir() {
		fmt.Fprintf(os.Stderr, "install-remote.sh not found at %s — run ./install.sh to install Teamster first\n", scriptPath)
		return 1
	}
	if fi.Mode()&0o111 == 0 {
		fmt.Fprintf(os.Stderr, "install-remote.sh not found at %s — run ./install.sh to install Teamster first\n", scriptPath)
		return 1
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bash not found\n")
		return 1
	}

	env := append(os.Environ(), "TEAMSTER_BASEDIR="+basedir)
	argv := append([]string{"bash", scriptPath}, args...)

	// Replace this process with bash running the script.
	if err := syscall.Exec(bash, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "exec failed: %v\n", err)
		return 1
	}
	return 0 // unreachable after successful exec
}
