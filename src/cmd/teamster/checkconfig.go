package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/intercept"
)

// runCheckConfig validates $BASEDIR/etc/interceptors.yaml without requiring
// a hookd restart, so an operator can check an edit before applying it.
func runCheckConfig(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-config: %v\n", err)
		return 1
	}
	// DataDir is <basedir>/var (same convention as grafanaBasedir/prometheusBasedir).
	basedir := filepath.Dir(cfg.DataDir)
	path := filepath.Join(basedir, "etc", "interceptors.yaml")

	reg, err := intercept.Load(path)
	if err != nil {
		fmt.Printf("interceptors.yaml: invalid\n  %v\n", err)
		return 1
	}

	rules := 0
	for _, ns := range reg.Namespaces {
		rules += len(ns.Rules)
	}
	fmt.Printf("interceptors.yaml: valid\n  %d tags, %d namespaces, %d rules\n",
		len(reg.Tags), len(reg.Namespaces), rules)
	return 0
}
