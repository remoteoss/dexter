package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/store"
)

const workspaceToolName = "dexter_workspace"

// ProjectParam is the project argument of every tool. A session whose root
// holds several projects (clones and worktrees of other repositories, see
// store.IsProjectContainer) has no index of its own, so each call names the
// project it asks about. In any session, a call can name another project.
type ProjectParam struct {
	Project string `json:"project,omitempty" jsonschema:"the project to answer from: its directory, or any file or directory in it (absolute, or relative to the session's root). Needed only when the session's root holds several projects, such as clones and worktrees; omit it otherwise"`
}

func (p ProjectParam) projectPath() string { return p.Project }

type projectPather interface{ projectPath() string }

// containerPather is an argument that names a project only in a session
// whose root holds several projects (the file of dexter_file_outline).
type containerPather interface{ containerPath() string }

// projectPathOf is the project that the arguments name, or "", and the path
// that names it only in a container session.
func projectPathOf(args any) (project, inContainer string) {
	if p, ok := args.(projectPather); ok {
		project = strings.TrimSpace(p.projectPath())
	}
	if p, ok := args.(containerPather); ok {
		inContainer = p.containerPath()
	}
	return project, inContainer
}

// errContainerRoot is backendFor's answer for a session whose root holds
// several projects; callBackend turns it into a containerError.
var errContainerRoot = errors.New("the session's root holds several projects")

// containerScan is the result of store.ContainerProjects for one root.
type containerScan struct{ projects []string }

// isContainer reports whether root only holds other projects. The scan reads
// directories (about 0.1 s on a folder of 125 repositories), so it runs once
// for each root that a session resolves.
func (f *Frontend) isContainer(root string) bool { return f.containerOf(root) != nil }

func (f *Frontend) containerOf(root string) *containerScan {
	f.mu.Lock()
	scan, ok := f.containers[root]
	f.mu.Unlock()
	if ok {
		return scan
	}
	if projects, isContainer := store.ContainerProjects(root); isContainer && len(projects) > 0 {
		scan = &containerScan{projects: projects}
	}
	f.mu.Lock()
	if f.containers != nil {
		f.containers[root] = scan
	}
	f.mu.Unlock()
	return scan
}

// callBackend returns the Backend for one tool call, and a note to put before
// its answer. With no project, it is the session's Backend. A session whose
// root is a container uses its only indexed project, and says so; with no
// such project, the call fails with the list of projects. A project path goes
// to the project that holds it.
func (f *Frontend) callBackend(ctx context.Context, ss *mcp.ServerSession, project, inContainer string) (Backend, string, error) {
	b, err := f.backendFor(ctx, ss)
	isContainer := errors.Is(err, errContainerRoot)
	if err != nil && !isContainer {
		return nil, "", err
	}
	if project == "" && isContainer {
		project = inContainer
	}
	if project == "" {
		if !isContainer {
			return b, "", nil
		}
		ce := f.newContainerError(f.sessionRoot(ss))
		if len(ce.indexed) != 1 {
			return nil, "", ce
		}
		only := ce.indexed[0]
		pb, err := f.projectBackend(ss, only)
		if err != nil {
			return nil, "", err
		}
		return pb, fmt.Sprintf("Note: %s holds several projects; this answer is from %s, the only one with a Dexter index. Pass project to ask another one.", ce.root, only), nil
	}

	base := f.sessionRoot(ss)
	path := project
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	info, statErr := os.Stat(path)
	if statErr != nil {
		return nil, "", fmt.Errorf("project %s: %w", project, statErr)
	}
	dir := path
	if !info.IsDir() {
		dir = filepath.Dir(path)
	}
	// Below the session's root, the project is the nearest nested clone or
	// linked worktree, as for the editor (store.NestedProject), else the
	// session's root. Elsewhere it is found as for any root.
	var root string
	if nested := store.NestedProject(dir, base); nested != "" {
		root = nested
	} else if rel, relErr := filepath.Rel(base, dir); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		root = base
	} else if root, err = f.resolve(dir); err != nil {
		return nil, "", fmt.Errorf("project %s: %w", project, err)
	}
	if !isContainer && (root == base || store.SameDir(root, base)) {
		return b, "", nil
	}
	if f.isContainer(root) {
		return nil, "", f.newContainerError(root)
	}
	pb, err := f.projectBackend(ss, root)
	return pb, "", err
}

// connKey is the key of root's connection: the folder in its canonical
// spelling, so that a symlink or other letter case shares one connection
// (a second connection in another spelling would fail the daemon's root
// check). A folder that does not exist keys as written.
func connKey(root string) string {
	if c := store.CanonicalDir(root); c != "" {
		return c
	}
	return root
}

// sessionRoot is the root that backendFor bound the session to.
func (f *Frontend) sessionRoot(ss *mcp.ServerSession) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.sessions[ss]; ok {
		return st.root
	}
	return f.cfg.Root
}

// projectBackend returns the Backend of root for the session, which then
// holds one user of its connection until it ends.
func (f *Frontend) projectBackend(ss *mcp.ServerSession, root string) (Backend, error) {
	key := connKey(root)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, errClosed
	}
	st, ok := f.sessions[ss]
	if !ok {
		f.mu.Unlock()
		return nil, errClosed
	}
	if !st.container && st.key == key {
		b := f.conns[key].backend
		f.mu.Unlock()
		return b, nil
	}
	c, exists := f.conns[key]
	if !exists {
		c = &workspaceConn{backend: f.cfg.Connect(root)}
		f.conns[key] = c
	}
	if !st.projects[key] {
		if st.projects == nil {
			st.projects = make(map[string]bool)
		}
		st.projects[key] = true
		c.users++
	}
	f.mu.Unlock()
	if !exists {
		log.Printf("MCP session project: %s", root)
		if w, ok := c.backend.(warmer); ok && store.NonProjectRootError(root) == nil {
			go w.warm(context.Background())
		}
	}
	return c.backend, nil
}

// maxListedProjects bounds the projects that a container error lists.
const maxListedProjects = 40

// containerError is the answer to a call without a project in a session
// whose root holds several projects.
type containerError struct {
	root      string
	indexed   []string // the projects that have a Dexter index
	unindexed []string
}

// newContainerError lists the projects of the container root, each with
// whether it has an index now (one stat each).
func (f *Frontend) newContainerError(root string) *containerError {
	e := &containerError{root: root}
	scan := f.containerOf(root)
	if scan == nil {
		return e
	}
	for _, p := range scan.projects {
		if store.HasIndex(p) {
			e.indexed = append(e.indexed, p)
		} else {
			e.unindexed = append(e.unindexed, p)
		}
	}
	return e
}

func (e *containerError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is not one Elixir project: it holds %d projects (clones and worktrees). Pass project with the directory of the project you work in, or any file in it, for example project: %q.", e.root, len(e.indexed)+len(e.unindexed), e.example())
	e.list(&b)
	return b.String()
}

// overview is the dexter_workspace answer for the container.
func (e *containerError) overview() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace root: %s\nThis directory holds %d Elixir projects (clones and worktrees of other repositories), and has no index of its own. Every tool takes a project argument: the directory of the project you work in, or any file in it, for example project: %q. Call dexter_workspace with a project for its overview.", e.root, len(e.indexed)+len(e.unindexed), e.example())
	e.list(&b)
	return b.String()
}

func (e *containerError) example() string {
	all := append(append([]string(nil), e.indexed...), e.unindexed...)
	if len(all) == 0 {
		return e.root
	}
	if rel, err := filepath.Rel(e.root, all[0]); err == nil {
		return rel
	}
	return all[0]
}

func (e *containerError) list(b *strings.Builder) {
	n := 0
	write := func(title string, paths []string) {
		if len(paths) == 0 || n >= maxListedProjects {
			return
		}
		fmt.Fprintf(b, "\n\n%s:", title)
		for _, p := range paths {
			if n >= maxListedProjects {
				fmt.Fprintf(b, "\n- ... and %d more", len(e.indexed)+len(e.unindexed)-n)
				return
			}
			rel, err := filepath.Rel(e.root, p)
			if err != nil {
				rel = p
			}
			fmt.Fprintf(b, "\n- %s", rel)
			n++
		}
	}
	write("Projects with a Dexter index", e.indexed)
	write("Projects without an index (the first call that names one indexes it, which can take a minute on a large project)", e.unindexed)
}
