package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/wms"
	"github.com/bmjdotnet/teamster/internal/store/mysql"
)

// runTags dispatches the `teamster tags <subcommand>` family. Returns the exit code.
func runTags(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, tagsUsage)
		return 2
	}
	switch args[0] {
	case "list":
		return runTagsList(args[1:])
	case "add-key":
		return runTagsAddKey(args[1:])
	case "add-value":
		return runTagsAddValue(args[1:])
	case "retire":
		return runTagsRetire(args[1:])
	case "retire-value":
		return runTagsRetireValue(args[1:])
	case "delete":
		return runTagsDelete(args[1:])
	case "delete-value":
		return runTagsDeleteValue(args[1:])
	case "describe":
		return runTagsDescribe(args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, tagsUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown tags subcommand: %s\n%s\n", args[0], tagsUsage)
		return 2
	}
}

const tagsUsage = `usage: teamster tags <subcommand>

subcommands:
  list                        list all tag keys (or values with --key)
  add-key <key>               add a new tag key to the vocabulary
  add-value <key>:<value>     add a new value to an existing key
  retire <key>                demote a key from the seed vocabulary (non-destructive)
  retire-value <key>:<value>  retire a specific value (hides it from list by default)
  delete <key>                DELETE a key and all its values (destructive, cascades to entity bindings)
  delete-value <key>:<value>  DELETE a specific value (destructive, cascades to entity bindings)
  describe <key> "<desc>"     update the description on a tag key

flags (add-key):
  --category <context|lifecycle>    default: context
  --cardinality <single|multi>      default: single
  --description "<text>"            optional description

flags (add-value):
  --description "<text>"            optional description

flags (list):
  --key <key>                       show values for a specific key
  --show-retired                    include retired values in output`

func openTagsDB() (*mysql.Store, error) {
	dsn := os.Getenv("TEAMSTER_STORE_DSN")
	if dsn == "" {
		// Fall back to DSN from teamster.yaml via config.Load so the CLI works
		// without the env var in the user's shell (managed-mode installs only set
		// the env in settings.json for hooks/MCPs, not in the shell).
		cfg, err := config.Load()
		if err == nil && cfg.StoreDSN.Primary != "" {
			dsn = cfg.StoreDSN.Primary
		}
	}
	if dsn == "" {
		return nil, fmt.Errorf("TEAMSTER_STORE_DSN is not set and no DSN found in teamster.yaml")
	}
	parsed, err := config.ParseStoreDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	if parsed.Driver != config.StoreDriverMySQL {
		return nil, fmt.Errorf("unsupported store driver: %s", parsed.Driver)
	}
	return mysql.New(parsed.Primary)
}

func runTagsList(args []string) int {
	fs := flag.NewFlagSet("teamster tags list", flag.ContinueOnError)
	key := fs.String("key", "", "show values for a specific key")
	showRetired := fs.Bool("show-retired", false, "include retired values in output")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags list: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()
	tags, err := s.ListTags(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags list: %v\n", err)
		return 1
	}

	if !*showRetired {
		filtered := tags[:0]
		for _, t := range tags {
			if !t.Retired {
				filtered = append(filtered, t)
			}
		}
		tags = filtered
	}

	counts, err := queryEntityCounts(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags list: entity counts: %v\n", err)
		return 1
	}

	if *key != "" {
		return printTagValues(tags, counts, *key)
	}
	return printTagKeys(tags, counts)
}

// entityCountKey is a composite key for entity_tags counts.
type entityCountKey struct {
	tagKey   string
	tagValue string
}

func queryEntityCounts(s *mysql.Store) (map[entityCountKey]int, error) {
	db := s.DB()
	rows, err := db.QueryContext(context.Background(), `
		SELECT t.tag_key, t.tag_value, COUNT(et.tag_id) AS cnt
		FROM tags t
		LEFT JOIN entity_tags et ON et.tag_id = t.id
		GROUP BY t.id, t.tag_key, t.tag_value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := map[entityCountKey]int{}
	for rows.Next() {
		var k, v string
		var cnt int
		if err := rows.Scan(&k, &v, &cnt); err != nil {
			return nil, err
		}
		out[entityCountKey{k, v}] = cnt
	}
	return out, rows.Err()
}

func printTagKeys(tags []wms.Tag, counts map[entityCountKey]int) int {
	// Aggregate entity count per key (sum across all values).
	keyCount := map[string]int{}
	seen := map[string]bool{}
	for _, t := range tags {
		keyCount[t.Key] += counts[entityCountKey{t.Key, t.Value}]
		seen[t.Key] = true
	}

	// Print one row per unique key (first occurrence carries the key metadata).
	printed := map[string]bool{}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tCATEGORY\tCARD\tSEED\tENTITIES\tDESCRIPTION")
	for _, t := range tags {
		if printed[t.Key] {
			continue
		}
		printed[t.Key] = true
		seed := "no"
		if t.IsSeed {
			seed = "yes"
		}
		desc := t.Description
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			t.Key, t.Category, t.Cardinality, seed, keyCount[t.Key], desc)
	}
	w.Flush() //nolint:errcheck
	return 0
}

func printTagValues(tags []wms.Tag, counts map[entityCountKey]int, key string) int {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "KEY\tVALUE\tSEED\tENTITIES\tDESCRIPTION\n")
	found := false
	for _, t := range tags {
		if t.Key != key {
			continue
		}
		found = true
		seed := "no"
		if t.IsSeed {
			seed = "yes"
		}
		value := t.Value
		if value == "" {
			value = "(stub)"
		}
		desc := t.Description
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
			t.Key, value, seed, counts[entityCountKey{t.Key, t.Value}], desc)
	}
	w.Flush() //nolint:errcheck
	if !found {
		fmt.Fprintf(os.Stderr, "tags list: key %q not found in vocabulary\n", key)
		return 1
	}
	return 0
}

func runTagsAddKey(args []string) int {
	fs := flag.NewFlagSet("teamster tags add-key", flag.ContinueOnError)
	category := fs.String("category", "context", "tag category: context or lifecycle")
	cardinality := fs.String("cardinality", "single", "tag cardinality: single or multi")
	description := fs.String("description", "", "description of this tag key")
	integration := fs.Bool("integration", false, "allow dotted namespace (integration key)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: teamster tags add-key <key> [flags]")
		return 2
	}
	key := fs.Arg(0)

	if strings.Contains(key, ".") && !*integration {
		fmt.Fprintf(os.Stderr, "tags add-key: %q contains a dotted namespace reserved for integration keys\n", key)
		fmt.Fprintln(os.Stderr, "  pass --integration to create an integration key")
		return 1
	}
	if *category != "context" && *category != "lifecycle" {
		fmt.Fprintf(os.Stderr, "tags add-key: --category must be 'context' or 'lifecycle', got %q\n", *category)
		return 1
	}
	if *cardinality != "single" && *cardinality != "multi" {
		fmt.Fprintf(os.Stderr, "tags add-key: --cardinality must be 'single' or 'multi', got %q\n", *cardinality)
		return 1
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags add-key: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	spec := wms.TagSpec{
		Key:         key,
		Category:    *category,
		Cardinality: *cardinality,
		Description: *description,
	}
	if err := s.DefineTag(context.Background(), spec); err != nil {
		fmt.Fprintf(os.Stderr, "tags add-key: %v\n", err)
		return 1
	}
	fmt.Printf("Added key %q (%s, %s)\n", key, *category, *cardinality)
	return 0
}

func runTagsAddValue(args []string) int {
	fs := flag.NewFlagSet("teamster tags add-value", flag.ContinueOnError)
	description := fs.String("description", "", "description for this value")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: teamster tags add-value <key>:<value> [--description \"...\"]")
		return 2
	}
	kv := fs.Arg(0)
	colon := strings.IndexByte(kv, ':')
	if colon < 0 {
		fmt.Fprintf(os.Stderr, "tags add-value: argument must be key:value, got %q\n", kv)
		return 1
	}
	key := kv[:colon]
	value := kv[colon+1:]
	if key == "" || value == "" {
		fmt.Fprintf(os.Stderr, "tags add-value: key and value must both be non-empty in %q\n", kv)
		return 1
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags add-value: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	db := s.DB()
	ctx := context.Background()

	// Verify the key exists.
	var exists int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE tag_key = ?`, key).Scan(&exists)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags add-value: %v\n", err)
		return 1
	}
	if exists == 0 {
		fmt.Fprintf(os.Stderr, "tags add-value: key %q does not exist; create it first with 'teamster tags add-key'\n", key)
		return 1
	}

	// Fetch category and cardinality from the existing key.
	var category, cardinality string
	err = db.QueryRowContext(ctx,
		`SELECT category, cardinality FROM tags WHERE tag_key = ? LIMIT 1`, key,
	).Scan(&category, &cardinality)
	if err != nil && err != sql.ErrNoRows {
		fmt.Fprintf(os.Stderr, "tags add-value: %v\n", err)
		return 1
	}

	_, err = db.ExecContext(ctx,
		`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
		 VALUES (?, ?, 0, ?, ?, ?)`,
		key, value, category, cardinality, *description,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags add-value: %v\n", err)
		return 1
	}
	fmt.Printf("Added value %q to key %q\n", value, key)
	return 0
}

func runTagsRetire(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: teamster tags retire <key>")
		return 2
	}
	key := args[0]

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags retire: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()

	// Count values and entity bindings before retiring.
	db := s.DB()
	var valueCount, bindingCount int
	db.QueryRowContext(ctx, //nolint:errcheck
		`SELECT COUNT(*) FROM tags WHERE tag_key = ?`, key,
	).Scan(&valueCount) //nolint:errcheck
	db.QueryRowContext(ctx, //nolint:errcheck
		`SELECT COUNT(*) FROM entity_tags WHERE tag_id IN (SELECT id FROM tags WHERE tag_key = ?)`, key,
	).Scan(&bindingCount) //nolint:errcheck

	if err := s.RetireTag(ctx, key); err != nil {
		fmt.Fprintf(os.Stderr, "tags retire: %v\n", err)
		return 1
	}
	fmt.Printf("Retired key %q (%d values demoted, %d entity bindings preserved)\n",
		key, valueCount, bindingCount)
	return 0
}

func runTagsRetireValue(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: teamster tags retire-value <key>:<value>")
		return 2
	}
	kv := args[0]
	colon := strings.IndexByte(kv, ':')
	if colon < 0 {
		fmt.Fprintf(os.Stderr, "tags retire-value: argument must be key:value, got %q\n", kv)
		return 1
	}
	key := kv[:colon]
	value := kv[colon+1:]
	if key == "" || value == "" {
		fmt.Fprintf(os.Stderr, "tags retire-value: key and value must both be non-empty in %q\n", kv)
		return 1
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags retire-value: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	if err := s.RetireTagValue(context.Background(), key, value); err != nil {
		fmt.Fprintf(os.Stderr, "tags retire-value: %v\n", err)
		return 1
	}
	fmt.Printf("Retired value %q from key %q\n", value, key)
	return 0
}

func runTagsDelete(args []string) int {
	var force bool
	var key string
	for _, a := range args {
		switch a {
		case "--force", "-force":
			force = true
		default:
			if key != "" {
				fmt.Fprintln(os.Stderr, "usage: teamster tags delete <key> [--force]")
				return 2
			}
			key = a
		}
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "usage: teamster tags delete <key> [--force]")
		return 2
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags delete: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	db := s.DB()
	ctx := context.Background()

	var valueCount, bindingCount int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE tag_key = ?`, key).Scan(&valueCount)         //nolint:errcheck
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity_tags WHERE tag_id IN (SELECT id FROM tags WHERE tag_key = ?)`, key).Scan(&bindingCount) //nolint:errcheck

	if valueCount == 0 {
		fmt.Fprintf(os.Stderr, "tags delete: key %q not found\n", key)
		return 1
	}

	if bindingCount > 0 && !force {
		fmt.Fprintf(os.Stderr, "tags delete: key %q has %d entity binding(s) — pass --force to delete anyway\n", key, bindingCount)
		return 1
	}

	res, err := db.ExecContext(ctx, `DELETE FROM tags WHERE tag_key = ?`, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags delete: %v\n", err)
		return 1
	}
	n, _ := res.RowsAffected()
	fmt.Printf("Deleted key %q (%d value(s) removed, %d entity binding(s) cascaded)\n", key, n, bindingCount)
	return 0
}

func runTagsDeleteValue(args []string) int {
	var force bool
	var kv string
	for _, a := range args {
		switch a {
		case "--force", "-force":
			force = true
		default:
			if kv != "" {
				fmt.Fprintln(os.Stderr, "usage: teamster tags delete-value <key>:<value> [--force]")
				return 2
			}
			kv = a
		}
	}
	if kv == "" {
		fmt.Fprintln(os.Stderr, "usage: teamster tags delete-value <key>:<value> [--force]")
		return 2
	}
	colon := strings.IndexByte(kv, ':')
	if colon < 0 {
		fmt.Fprintf(os.Stderr, "tags delete-value: argument must be key:value, got %q\n", kv)
		return 1
	}
	key := kv[:colon]
	value := kv[colon+1:]
	if key == "" || value == "" {
		fmt.Fprintf(os.Stderr, "tags delete-value: key and value must both be non-empty in %q\n", kv)
		return 1
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags delete-value: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	db := s.DB()
	ctx := context.Background()

	var tagID int64
	var bindingCount int
	err = db.QueryRowContext(ctx, `SELECT id FROM tags WHERE tag_key = ? AND tag_value = ?`, key, value).Scan(&tagID)
	if err != nil {
		if err == sql.ErrNoRows {
			fmt.Fprintf(os.Stderr, "tags delete-value: %s:%s not found\n", key, value)
			return 1
		}
		fmt.Fprintf(os.Stderr, "tags delete-value: %v\n", err)
		return 1
	}
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity_tags WHERE tag_id = ?`, tagID).Scan(&bindingCount) //nolint:errcheck

	if bindingCount > 0 && !force {
		fmt.Fprintf(os.Stderr, "tags delete-value: %s:%s has %d entity binding(s) — pass --force to delete anyway\n", key, value, bindingCount)
		return 1
	}

	_, err = db.ExecContext(ctx, `DELETE FROM tags WHERE id = ?`, tagID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags delete-value: %v\n", err)
		return 1
	}
	fmt.Printf("Deleted %s:%s (%d entity binding(s) cascaded)\n", key, value, bindingCount)
	return 0
}

func runTagsDescribe(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: teamster tags describe <key> \"<description>\"")
		return 2
	}
	key := args[0]
	desc := args[1]

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags describe: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	db := s.DB()
	ctx := context.Background()
	res, err := db.ExecContext(ctx,
		`UPDATE tags SET description = ? WHERE tag_key = ?`, desc, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tags describe: %v\n", err)
		return 1
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fmt.Fprintf(os.Stderr, "tags describe: key %q not found\n", key)
		return 1
	}
	fmt.Printf("Updated description on %d row(s) for key %q\n", n, key)
	return 0
}
