package tui

// IntegrationKey is a single key seeded by an integration.
type IntegrationKey struct {
	Key  string
	Desc string
}

// IntegrationDef describes an integration for the wizard.
type IntegrationDef struct {
	Name        string
	Description string
	Keys        []IntegrationKey
}

// Integrations is the ordered list of supported integrations.
var Integrations = []IntegrationDef{
	{
		Name:        "GitHub",
		Description: "Track work via GitHub PRs and issues. Seeds keys for owner, repo, PR number, issue number, and milestone.",
		Keys: []IntegrationKey{
			{"github.owner", "GitHub repository owner or organization name."},
			{"github.repo", "GitHub repository name."},
			{"github.pr", "GitHub pull request number."},
			{"github.issue", "GitHub issue number."},
			{"github.milestone", "GitHub milestone name or number."},
		},
	},
	{
		Name:        "GitLab",
		Description: "Track work via GitLab merge requests and issues. Seeds keys for group, project, MR number, issue number, and milestone.",
		Keys: []IntegrationKey{
			{"gitlab.group", "GitLab group or namespace."},
			{"gitlab.project", "GitLab project path (group/project)."},
			{"gitlab.mr", "GitLab merge request number."},
			{"gitlab.issue", "GitLab issue number."},
			{"gitlab.milestone", "GitLab milestone name."},
		},
	},
	{
		Name:        "Jira",
		Description: "Link work to Jira tickets, epics, and sprints. Seeds keys for project, issue ID, epic, sprint, and fix version.",
		Keys: []IntegrationKey{
			{"jira.project", "Jira project key (e.g. PROJ)."},
			{"jira.id", "Jira issue key (e.g. PROJ-123)."},
			{"jira.epic", "Jira epic key or name."},
			{"jira.sprint", "Jira sprint name or ID."},
			{"jira.fix-version", "Jira fix version / release target."},
		},
	},
	{
		Name:        "Local Git",
		Description: "Track work by repository and branch. Useful when no issue tracker exists.",
		Keys: []IntegrationKey{
			{"git.repo", "Local git repository path."},
			{"git.remote", "Git remote name (e.g. origin)."},
			{"git.branch", "Git branch name."},
		},
	},
	{
		Name:        "Redmine",
		Description: "Link work to Redmine issues and versions. Seeds keys for project, issue ID, tracker, and version.",
		Keys: []IntegrationKey{
			{"redmine.project", "Redmine project identifier."},
			{"redmine.id", "Redmine issue number."},
			{"redmine.tracker", "Redmine tracker type (bug, feature, etc.)."},
			{"redmine.version", "Redmine target version."},
		},
	},
	{
		Name:        "OpenProject",
		Description: "Link work to OpenProject work packages. Seeds keys for project, work package ID, type, and version.",
		Keys: []IntegrationKey{
			{"openproject.project", "OpenProject project name."},
			{"openproject.wp", "OpenProject work package ID."},
			{"openproject.type", "OpenProject work package type."},
			{"openproject.version", "OpenProject version / sprint."},
		},
	},
	{
		Name:        "Plane",
		Description: "Link work to Plane issues, cycles, and modules. Seeds keys for workspace, project, issue, cycle, and module.",
		Keys: []IntegrationKey{
			{"plane.workspace", "Plane workspace slug."},
			{"plane.project", "Plane project identifier."},
			{"plane.issue", "Plane issue identifier."},
			{"plane.cycle", "Plane cycle name."},
			{"plane.module", "Plane module name."},
		},
	},
	{
		Name:        "Taiga",
		Description: "Link work to Taiga user stories, sprints, and epics. Seeds keys for project, user story, sprint, and epic.",
		Keys: []IntegrationKey{
			{"taiga.project", "Taiga project slug."},
			{"taiga.us", "Taiga user story number."},
			{"taiga.sprint", "Taiga sprint name."},
			{"taiga.epic", "Taiga epic identifier."},
		},
	},
}

// UniversalKey defines one universal context key seeded for all installs.
type UniversalKey struct {
	Key         string
	Cardinality string
	Description string
	Summary     string // short line for Screen 3 display
}

// UniversalKeys are always seeded regardless of integration choices.
var UniversalKeys = []UniversalKey{
	{"product", "single", "The ongoing product or area of work. Primary aggregation axis.", "The product or area of work (e.g. teamster, homelab)"},
	{"feature", "single", "The specific feature being built.", "The specific feature being built (short slug)"},
	{"bug", "single", "The specific bug being fixed.", "The specific bug being fixed (short slug)"},
	{"component", "single", "Subsystem within a product (e.g. networking, harness, ui).", "Subsystem within a product (e.g. networking, ui)"},
	{"priority", "single", "Urgency: p0=critical, p1=high, p2=normal, p3=low.", "Urgency: p0=critical, p1=high, p2=normal, p3=low"},
	{"product-version", "single", "Version or milestone being targeted (semver or milestone slug).", "Version or milestone (e.g. 2.4.0, v0.1)"},
}

// LifecycleTag describes an engine-managed lifecycle tag.
type LifecycleTag struct {
	Key    string
	Desc   string
	Values string
}

// LifecycleTags are the engine-managed tags shown in Screen 7.
var LifecycleTags = []LifecycleTag{
	{"phase", "Current lifecycle phase of an entity.", "design, build, test, review, rework"},
	{"work-type", "Nature of the work being done.", "feature, bug, refactor, infra, research, test, docs"},
	{"resolution", "Terminal outcome when an entity reaches done.", "achieved, abandoned"},
	{"lifecycle", "Archival marker for entities that are no longer active.", "archived"},
}
