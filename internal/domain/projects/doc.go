// Package projects owns projects, project managers, favorites, backgrounds,
// the project-wide labels toggle, base custom-field groups (project-level
// templates) and the home / project UI pages.
//
// Exported Go API for other domains:
//
//	Get(e, id) (Project, error)            // authz-checked (View) project read
//	List(e, ListOpts) ([]Project, error)   // projects visible to e.User
//	IsManager(e, projectID) bool           // admin or project manager
//	BackgroundCSS(p Project) template.CSS  // inline style for a project background
//	FilesDir(e) (string, error)            // $WEAVE_DATA_DIR/files
package projects
