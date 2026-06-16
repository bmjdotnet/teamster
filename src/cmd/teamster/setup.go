package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/tui"
)

// runSetup dispatches `teamster setup <subcommand>`. Returns exit code.
func runSetup(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, setupUsage)
		return 2
	}
	switch args[0] {
	case "tags":
		return runSetupTags(args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, setupUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "setup %s: coming soon\n", args[0])
		return 1
	}
}

const setupUsage = `usage: teamster setup <subcommand>

subcommands:
  tags    Guided tag vocabulary setup and editor

flags (tags):
  --interview    Re-run the guided setup interview`

type integrationKey struct {
	key  string
	desc string
}

// integrationKeys maps a display name to the keys it seeds.
var integrationKeys = map[string][]integrationKey{
	"GitHub": {
		{"github.owner", "GitHub repository owner or organization name."},
		{"github.repo", "GitHub repository name."},
		{"github.pr", "GitHub pull request number."},
		{"github.issue", "GitHub issue number."},
		{"github.milestone", "GitHub milestone name or number."},
	},
	"GitLab": {
		{"gitlab.group", "GitLab group or namespace."},
		{"gitlab.project", "GitLab project path (group/project)."},
		{"gitlab.mr", "GitLab merge request number."},
		{"gitlab.issue", "GitLab issue number."},
		{"gitlab.milestone", "GitLab milestone name."},
	},
	"Jira": {
		{"jira.id", "Jira issue key (e.g. PROJ-123)."},
		{"jira.project", "Jira project key (e.g. PROJ)."},
		{"jira.epic", "Jira epic key or name."},
		{"jira.sprint", "Jira sprint name or ID."},
		{"jira.fix-version", "Jira fix version / release target."},
	},
	"Local Git": {
		{"git.repo", "Local git repository path."},
		{"git.branch", "Git branch name."},
		{"git.remote", "Git remote name (e.g. origin)."},
	},
	"Redmine": {
		{"redmine.project", "Redmine project identifier."},
		{"redmine.id", "Redmine issue number."},
		{"redmine.tracker", "Redmine tracker type (bug, feature, etc.)."},
		{"redmine.version", "Redmine target version."},
	},
	"OpenProject": {
		{"openproject.project", "OpenProject project name."},
		{"openproject.wp", "OpenProject work package ID."},
		{"openproject.type", "OpenProject work package type."},
		{"openproject.version", "OpenProject version / sprint."},
	},
	"Plane": {
		{"plane.workspace", "Plane workspace slug."},
		{"plane.project", "Plane project identifier."},
		{"plane.issue", "Plane issue identifier."},
		{"plane.cycle", "Plane cycle name."},
		{"plane.module", "Plane module name."},
	},
	"Taiga": {
		{"taiga.project", "Taiga project slug."},
		{"taiga.us", "Taiga user story number."},
		{"taiga.sprint", "Taiga sprint name."},
		{"taiga.epic", "Taiga epic identifier."},
	},
}

// integrationOrder is the display order for the integration menu.
var integrationOrder = []string{
	"GitHub", "GitLab", "Jira", "Local Git", "Redmine", "OpenProject", "Plane", "Taiga",
}

// universalContextKeys are always-seeded context keys (not lifecycle keys, which
// are owned by migrations and managed by the system).
var universalContextKeys = []struct {
	key         string
	cardinality string
	description string
}{
	{"product", "single", "The ongoing product or area of work. Primary aggregation axis."},
	{"feature", "single", "The specific feature being built."},
	{"bug", "single", "The specific bug being fixed."},
	{"component", "single", "Subsystem within a product (e.g. networking, harness, ui)."},
	{"priority", "single", "Urgency: p0=critical, p1=high, p2=normal, p3=low."},
	{"product-version", "single", "Version or milestone being targeted (semver or milestone slug)."},
}

// setupTagsHint maps a store-open failure to a one-line remediation hint, or ""
// when none applies. It matches on the error text (the DSN itself is never in
// these messages — go-sql-driver prints "(using password: YES)", not the
// secret, and the DSN-parse path masks the scheme only).
func setupTagsHint(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "newer than this binary supports"):
		return "the deployed binary is older than the database schema — run ./install.sh to upgrade the binary"
	case strings.Contains(msg, "Access denied"):
		return "auth failed — ensure $TEAMSTER_STORE_DSN is set, or reinstall to refresh teamster.yaml"
	default:
		return ""
	}
}

func runSetupTags(args []string) int {
	fs := flag.NewFlagSet("teamster setup tags", flag.ContinueOnError)
	interview := fs.Bool("interview", false, "re-run the guided setup interview")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup tags: %v\n", err)
		if hint := setupTagsHint(err); hint != "" {
			fmt.Fprintf(os.Stderr, "  hint: %s\n", hint)
		}
		return 1
	}
	defer s.Close() //nolint:errcheck

	db := s.DB()
	ctx := context.Background()

	// Detect first-run: no non-empty product values in the vocabulary.
	var productValueCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tags WHERE tag_key = 'product' AND tag_value != ''`,
	).Scan(&productValueCount)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup tags: %v\n", err)
		return 1
	}

	if productValueCount == 0 || *interview {
		if err := tui.RunWizard(db); err != nil {
			fmt.Fprintf(os.Stderr, "setup tags: %v\n", err)
			return 1
		}
		return 0
	}

	if err := tui.RunEditor(db); err != nil {
		fmt.Fprintf(os.Stderr, "setup tags: %v\n", err)
		return 1
	}
	return 0
}

func runSetupInterview(ctx context.Context, s *mysql.Store, scanner *bufio.Scanner) int {
	fmt.Println("=== Teamster Tag Setup ===")
	fmt.Println()

	// Step 1: Products.
	fmt.Println("What products do you work on? (comma-separated slugs, e.g. teamster, homelab)")
	fmt.Print("> ")
	var products []string
	if scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		for _, p := range strings.Split(line, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				products = append(products, p)
			}
		}
	}

	// Step 2: Integrations.
	fmt.Println()
	fmt.Println("Which systems do you use? (enter numbers, comma-separated, or press Enter to skip)")
	for i, name := range integrationOrder {
		fmt.Printf("  %d. %s\n", i+1, name)
	}
	fmt.Print("> ")

	var selectedIntegrations []string
	if scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		for _, tok := range strings.Split(line, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			n, err := strconv.Atoi(tok)
			if err != nil || n < 1 || n > len(integrationOrder) {
				fmt.Fprintf(os.Stderr, "  skipping invalid choice: %q\n", tok)
				continue
			}
			selectedIntegrations = append(selectedIntegrations, integrationOrder[n-1])
		}
	}

	// Apply.
	fmt.Println()
	fmt.Println("Setting up tag vocabulary...")

	db := s.DB()
	keysSeeded := 0
	valuesCreated := 0

	// Seed universal context keys.
	for _, uk := range universalContextKeys {
		_, err := db.ExecContext(ctx,
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
			 VALUES (?, '', 1, 'context', ?, ?)`,
			uk.key, uk.cardinality, uk.description,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error seeding key %q: %v\n", uk.key, err)
			return 1
		}
		keysSeeded++
	}
	fmt.Printf("  + Universal keys: %s\n",
		strings.Join(func() []string {
			var ks []string
			for _, uk := range universalContextKeys {
				ks = append(ks, uk.key)
			}
			return ks
		}(), ", "))

	// Seed product values.
	if len(products) > 0 {
		for _, p := range products {
			_, err := db.ExecContext(ctx,
				`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
				 VALUES ('product', ?, 0, 'context', 'single', '')`,
				p,
			)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  error adding product %q: %v\n", p, err)
				return 1
			}
			valuesCreated++
		}
		fmt.Printf("  + product: %s\n", strings.Join(products, ", "))
	}

	// Seed integration keys.
	for _, name := range selectedIntegrations {
		ikeys := integrationKeys[name]
		var keyNames []string
		for _, ik := range ikeys {
			_, err := db.ExecContext(ctx,
				`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
				 VALUES (?, '', 1, 'context', 'single', ?)`,
				ik.key, ik.desc,
			)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  error seeding %s key %q: %v\n", name, ik.key, err)
				return 1
			}
			keysSeeded++
			keyNames = append(keyNames, ik.key)
		}
		fmt.Printf("  + %s keys: %s\n", name, strings.Join(keyNames, ", "))
	}

	fmt.Println()
	fmt.Printf("Done! %d keys seeded, %d product values created.\n", keysSeeded, valuesCreated)
	fmt.Println("Run 'teamster tags list' to see your vocabulary.")
	fmt.Println("Run 'teamster setup tags' to edit, or --interview to re-run setup.")
	return 0
}

func runSetupEditor(ctx context.Context, s *mysql.Store, scanner *bufio.Scanner) int {
	db := s.DB()
	for {
		// Count current vocabulary.
		var keyCount, valueCount int
		db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT tag_key) FROM tags`).Scan(&keyCount)         //nolint:errcheck
		db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE tag_value != ''`).Scan(&valueCount) //nolint:errcheck

		fmt.Println()
		fmt.Println("=== Tag Vocabulary Editor ===")
		fmt.Println()
		fmt.Printf("Current vocabulary: %d keys, %d values\n", keyCount, valueCount)
		fmt.Println()
		fmt.Println("  1. List all keys")
		fmt.Println("  2. Add a product")
		fmt.Println("  3. Add a key")
		fmt.Println("  4. Add a value to a key")
		fmt.Println("  5. Retire a key")
		fmt.Println("  6. Re-run setup interview")
		fmt.Println("  7. Exit")
		fmt.Println()
		fmt.Print("> ")

		if !scanner.Scan() {
			return 0
		}
		choice := strings.TrimSpace(scanner.Text())

		switch choice {
		case "1":
			runTagsList(nil)

		case "2":
			fmt.Print("Product slug: ")
			if !scanner.Scan() {
				continue
			}
			p := strings.TrimSpace(scanner.Text())
			if p == "" {
				continue
			}
			_, err := db.ExecContext(ctx,
				`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
				 VALUES ('product', ?, 0, 'context', 'single', '')`, p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			} else {
				fmt.Printf("Added product %q\n", p)
			}

		case "3":
			fmt.Print("Key name: ")
			if !scanner.Scan() {
				continue
			}
			key := strings.TrimSpace(scanner.Text())
			if key == "" {
				continue
			}
			fmt.Print("Category (context/lifecycle) [context]: ")
			scanner.Scan()
			cat := strings.TrimSpace(scanner.Text())
			if cat == "" {
				cat = "context"
			}
			fmt.Print("Cardinality (single/multi) [single]: ")
			scanner.Scan()
			card := strings.TrimSpace(scanner.Text())
			if card == "" {
				card = "single"
			}
			fmt.Print("Description (optional): ")
			scanner.Scan()
			desc := strings.TrimSpace(scanner.Text())
			code := runTagsAddKey([]string{"--category", cat, "--cardinality", card, "--description", desc, key})
			if code != 0 {
				fmt.Fprintln(os.Stderr, "add-key failed")
			}

		case "4":
			fmt.Print("Key:value (e.g. product:scrollz): ")
			if !scanner.Scan() {
				continue
			}
			kv := strings.TrimSpace(scanner.Text())
			if kv == "" {
				continue
			}
			code := runTagsAddValue([]string{kv})
			if code != 0 {
				fmt.Fprintln(os.Stderr, "add-value failed")
			}

		case "5":
			fmt.Print("Key to retire: ")
			if !scanner.Scan() {
				continue
			}
			key := strings.TrimSpace(scanner.Text())
			if key == "" {
				continue
			}
			code := runTagsRetire([]string{key})
			if code != 0 {
				fmt.Fprintln(os.Stderr, "retire failed")
			}

		case "6":
			return runSetupInterview(ctx, s, scanner)

		case "7", "q", "quit", "exit":
			return 0

		default:
			fmt.Fprintf(os.Stderr, "unknown choice: %q\n", choice)
		}
	}
}
