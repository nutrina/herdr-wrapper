// Package web serves the task manager UI: server-rendered pages with htmx
// for the few places that update without a full page load.
package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/nutrina/herdr-wrapper/internal/herdr"
	"github.com/nutrina/herdr-wrapper/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

var statusOrder = []string{
	store.StatusRunning, store.StatusInput, store.StatusQueued,
	store.StatusDraft, store.StatusDone, store.StatusFailed,
}

var statusLabels = map[string]string{
	store.StatusRunning: "Running",
	store.StatusInput:   "Needs input",
	store.StatusQueued:  "Queued",
	store.StatusDraft:   "Draft",
	store.StatusDone:    "Done",
	store.StatusFailed:  "Failed",
}

type server struct {
	store *store.Store
	pages map[string]*template.Template
	md    goldmark.Markdown
}

// Register adds the UI routes to mux.
func Register(mux *http.ServeMux, st *store.Store) error {
	funcs := template.FuncMap{
		"ago":         ago,
		"stamp":       func(t time.Time) string { return t.Format("Jan 2, 15:04") },
		"size":        humanSize,
		"statusLabel": func(s string) string { return statusLabels[s] },
	}

	pages := map[string]*template.Template{}
	for _, name := range []string{"list", "detail", "form"} {
		t, err := template.New(name).Funcs(funcs).ParseFS(assets, "templates/base.html", "templates/"+name+".html")
		if err != nil {
			return err
		}
		pages[name] = t
	}

	static, err := fs.Sub(assets, "static")
	if err != nil {
		return err
	}

	s := &server{
		store: st,
		pages: pages,
		md:    goldmark.New(goldmark.WithExtensions(extension.GFM)),
	}

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tasks", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /tasks", s.listTasks)
	mux.HandleFunc("GET /tasks/new", s.newTask)
	mux.HandleFunc("POST /tasks", s.createTask)
	mux.HandleFunc("GET /tasks/{id}", s.showTask)
	mux.HandleFunc("GET /tasks/{id}/edit", s.editTask)
	mux.HandleFunc("POST /tasks/{id}", s.updateTask)
	mux.HandleFunc("POST /tasks/{id}/memory", s.saveMemory)
	mux.HandleFunc("POST /tasks/{id}/runs", s.queueRun)
	mux.HandleFunc("POST /tasks/{id}/files", s.attachFiles)
	mux.HandleFunc("GET /tasks/{id}/files/{file}", s.downloadFile)
	mux.HandleFunc("POST /tasks/{id}/files/{file}/delete", s.deleteFile)
	mux.HandleFunc("POST /preview", s.preview)

	return nil
}

// page holds what the shared layout needs; every page's data embeds it.
type page struct {
	Crumb    string
	SocketOK bool
}

func newPage(crumb string) page {
	return page{Crumb: crumb, SocketOK: socketReachable()}
}

func socketReachable() bool {
	conn, err := net.DialTimeout("unix", herdr.SOCKET_FILE, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

type taskView struct {
	store.Task
}

func (t taskView) DisplayID() string {
	return fmt.Sprintf("T-%04d", t.ID)
}

func (t taskView) Project() string {
	project, _, _ := strings.Cut(t.Category, ":")
	return project
}

func (t taskView) Team() string {
	_, team, _ := strings.Cut(t.Category, ":")
	return team
}

type filterItem struct {
	Key    string
	Label  string
	Count  int
	URL    string
	Active bool
	Nested bool
}

type listData struct {
	page
	Status      string
	Category    string
	Label       string
	Query       string
	StatusItems []filterItem
	CatItems    []filterItem
	LabelItems  []filterItem
	Tasks       []taskView
	Total       int
	Filtered    bool
}

func listURL(status, category, label, query string) string {
	v := url.Values{}
	if status != "" {
		v.Set("status", status)
	}
	if category != "" {
		v.Set("cat", category)
	}
	if label != "" {
		v.Set("label", label)
	}
	if query != "" {
		v.Set("q", query)
	}
	if len(v) == 0 {
		return "/tasks"
	}
	return "/tasks?" + v.Encode()
}

// inCategory reports whether a task category matches a filter, which is
// either a full "project:team" or just a project.
func inCategory(category, filter string) bool {
	return filter == "" || category == filter || strings.HasPrefix(category, filter+":")
}

func hasLabel(labels []string, label string) bool {
	for _, l := range labels {
		if l == label {
			return true
		}
	}
	return false
}

func (s *server) listTasks(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListTasks()
	if err != nil {
		s.fail(w, err)
		return
	}

	q := r.URL.Query()
	status, category, label := q.Get("status"), q.Get("cat"), q.Get("label")
	query := strings.TrimSpace(q.Get("q"))

	data := listData{
		page:     newPage("tasks"),
		Status:   status,
		Category: category,
		Label:    label,
		Query:    query,
		Total:    len(all),
		Filtered: status != "" || category != "" || label != "" || query != "",
	}

	// Status filter, with the number of tasks in each status.
	statusCounts := map[string]int{}
	for _, t := range all {
		statusCounts[t.Status]++
	}
	data.StatusItems = append(data.StatusItems, filterItem{
		Key: "all", Label: "All tasks", Count: len(all),
		URL: listURL("", category, label, query), Active: status == "",
	})
	for _, key := range statusOrder {
		data.StatusItems = append(data.StatusItems, filterItem{
			Key: key, Label: statusLabels[key], Count: statusCounts[key],
			URL: listURL(key, category, label, query), Active: status == key,
		})
	}

	// Category filter: each project followed by its project:team categories.
	teams := map[string][]string{}
	seen := map[string]bool{}
	var projects []string
	for _, t := range all {
		if t.Category == "" || seen[t.Category] {
			continue
		}
		seen[t.Category] = true
		project, _, hasTeam := strings.Cut(t.Category, ":")
		if _, ok := teams[project]; !ok {
			teams[project] = nil
			projects = append(projects, project)
		}
		if hasTeam {
			teams[project] = append(teams[project], t.Category)
		}
	}
	sort.Strings(projects)

	countIn := func(filter string) int {
		n := 0
		for _, t := range all {
			if inCategory(t.Category, filter) {
				n++
			}
		}
		return n
	}
	data.CatItems = append(data.CatItems, filterItem{
		Label: "all", Count: len(all),
		URL: listURL(status, "", label, query), Active: category == "",
	})
	for _, project := range projects {
		data.CatItems = append(data.CatItems, filterItem{
			Label: project, Count: countIn(project),
			URL: listURL(status, project, label, query), Active: category == project,
		})
		sort.Strings(teams[project])
		for _, full := range teams[project] {
			data.CatItems = append(data.CatItems, filterItem{
				Label: strings.TrimPrefix(full, project), Count: countIn(full), Nested: true,
				URL: listURL(status, full, label, query), Active: category == full,
			})
		}
	}

	// Label filter: clicking the active label clears it.
	seen = map[string]bool{}
	var labels []string
	for _, t := range all {
		for _, l := range t.Labels {
			if !seen[l] {
				seen[l] = true
				labels = append(labels, l)
			}
		}
	}
	sort.Strings(labels)
	for _, l := range labels {
		item := filterItem{Label: l, URL: listURL(status, category, l, query), Active: label == l}
		if item.Active {
			item.URL = listURL(status, category, "", query)
		}
		data.LabelItems = append(data.LabelItems, item)
	}

	needle := strings.ToLower(query)
	for _, t := range all {
		if status != "" && t.Status != status {
			continue
		}
		if !inCategory(t.Category, category) {
			continue
		}
		if label != "" && !hasLabel(t.Labels, label) {
			continue
		}
		if needle != "" &&
			!strings.Contains(strings.ToLower(t.Title), needle) &&
			!strings.Contains(strings.ToLower(t.Description), needle) {
			continue
		}
		data.Tasks = append(data.Tasks, taskView{t})
	}

	s.render(w, http.StatusOK, "list", data)
}

type detailData struct {
	page
	Task        taskView
	Description template.HTML
	Memory      template.HTML
	Files       []store.Attachment
	Runs        []store.Run
}

func (s *server) showTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadTask(w, r)
	if !ok {
		return
	}

	files, err := s.store.ListAttachments(task.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	runs, err := s.store.ListRuns(task.ID)
	if err != nil {
		s.fail(w, err)
		return
	}

	view := taskView{*task}
	s.render(w, http.StatusOK, "detail", detailData{
		page:        newPage("tasks / " + view.DisplayID()),
		Task:        view,
		Description: s.markdown(task.Description),
		Memory:      s.markdown(task.Memory),
		Files:       files,
		Runs:        runs,
	})
}

type formData struct {
	page
	IsNew      bool
	Action     string
	Cancel     string
	Task       taskView
	Labels     string
	Files      []store.Attachment
	Categories []string
	Error      string
}

// formFor builds the data for the new/edit form around task.
func (s *server) formFor(task *store.Task, isNew bool) (formData, error) {
	view := taskView{*task}
	data := formData{
		IsNew:  isNew,
		Task:   view,
		Labels: strings.Join(task.Labels, ","),
	}

	if isNew {
		data.page = newPage("tasks / new")
		data.Action = "/tasks"
		data.Cancel = "/tasks"
	} else {
		data.page = newPage("tasks / " + view.DisplayID() + " / edit")
		data.Action = fmt.Sprintf("/tasks/%d", task.ID)
		data.Cancel = data.Action

		files, err := s.store.ListAttachments(task.ID)
		if err != nil {
			return data, err
		}
		data.Files = files
	}

	// Categories already in use, offered as suggestions.
	all, err := s.store.ListTasks()
	if err != nil {
		return data, err
	}
	seen := map[string]bool{}
	for _, t := range all {
		if t.Category != "" && !seen[t.Category] {
			seen[t.Category] = true
			data.Categories = append(data.Categories, t.Category)
		}
	}
	sort.Strings(data.Categories)

	return data, nil
}

func (s *server) newTask(w http.ResponseWriter, r *http.Request) {
	data, err := s.formFor(&store.Task{}, true)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "form", data)
}

func (s *server) editTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	data, err := s.formFor(task, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "form", data)
}

// readTaskForm copies the submitted form fields into task.
func readTaskForm(r *http.Request, task *store.Task) error {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return err
	}

	task.Title = strings.TrimSpace(r.FormValue("title"))
	task.Description = strings.ReplaceAll(r.FormValue("description"), "\r\n", "\n")

	// Normalise "crm : backend" or "crm:" so the list's project:team filter matches.
	project, team, _ := strings.Cut(r.FormValue("category"), ":")
	project, team = strings.TrimSpace(project), strings.TrimSpace(team)
	task.Category = project
	if project != "" && team != "" {
		task.Category = project + ":" + team
	}

	// "labels" holds the chips; "label_entry" is whatever was typed but not yet added.
	task.Labels = nil
	for _, l := range strings.Split(r.FormValue("labels")+","+r.FormValue("label_entry"), ",") {
		l = strings.TrimSpace(l)
		if l != "" && !hasLabel(task.Labels, l) {
			task.Labels = append(task.Labels, l)
		}
	}

	return nil
}

func (s *server) saveUploads(r *http.Request, taskID int64) error {
	if r.MultipartForm == nil {
		return nil
	}
	for _, header := range r.MultipartForm.File["files"] {
		f, err := header.Open()
		if err != nil {
			return err
		}
		err = s.store.SaveAttachment(taskID, header.Filename, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *server) createTask(w http.ResponseWriter, r *http.Request) {
	task := &store.Task{}
	if err := readTaskForm(r, task); err != nil {
		http.Error(w, "Could not read the form: "+err.Error(), http.StatusBadRequest)
		return
	}

	if task.Title == "" {
		s.renderFormError(w, task, true, "A task needs a title.")
		return
	}

	id, err := s.store.CreateTask(task)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.finishSave(w, r, id)
}

func (s *server) updateTask(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	if err := readTaskForm(r, task); err != nil {
		http.Error(w, "Could not read the form: "+err.Error(), http.StatusBadRequest)
		return
	}

	if task.Title == "" {
		s.renderFormError(w, task, false, "A task needs a title.")
		return
	}

	if err := s.store.UpdateTask(task); err != nil {
		s.fail(w, err)
		return
	}
	s.finishSave(w, r, task.ID)
}

// finishSave stores the uploaded files, queues a run if that button was used,
// and sends the browser to the task.
func (s *server) finishSave(w http.ResponseWriter, r *http.Request, taskID int64) {
	if err := s.saveUploads(r, taskID); err != nil {
		s.fail(w, err)
		return
	}
	if r.FormValue("action") == "run" {
		if err := s.store.QueueRun(taskID); err != nil {
			s.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/tasks/%d", taskID), http.StatusSeeOther)
}

func (s *server) renderFormError(w http.ResponseWriter, task *store.Task, isNew bool, message string) {
	data, err := s.formFor(task, isNew)
	if err != nil {
		s.fail(w, err)
		return
	}
	data.Error = message
	s.render(w, http.StatusUnprocessableEntity, "form", data)
}

func (s *server) saveMemory(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	memory := strings.ReplaceAll(r.FormValue("memory"), "\r\n", "\n")
	if err := s.store.SetMemory(task.ID, strings.TrimSpace(memory)); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/tasks/%d", task.ID), http.StatusSeeOther)
}

func (s *server) queueRun(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	if err := s.store.QueueRun(task.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/tasks/%d", task.ID), http.StatusSeeOther)
}

func (s *server) attachFiles(w http.ResponseWriter, r *http.Request) {
	task, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "Could not read the upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.saveUploads(r, task.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/tasks/%d", task.ID), http.StatusSeeOther)
}

func (s *server) downloadFile(w http.ResponseWriter, r *http.Request) {
	file, ok := s.loadFile(w, r)
	if !ok {
		return
	}
	// Always download: attachments are arbitrary files and must not run as pages of this origin.
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(file.Name))
	http.ServeFile(w, r, s.store.AttachmentPath(file))
}

// deleteFile answers with an empty body; the form page removes the row itself.
func (s *server) deleteFile(w http.ResponseWriter, r *http.Request) {
	file, ok := s.loadFile(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteAttachment(file.TaskID, file.ID); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// preview renders the posted markdown description as an HTML fragment.
func (s *server) preview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, s.markdown(r.FormValue("description")))
}

func (s *server) loadTask(w http.ResponseWriter, r *http.Request) (*store.Task, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	task, err := s.store.GetTask(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return task, true
}

func (s *server) loadFile(w http.ResponseWriter, r *http.Request) (*store.Attachment, bool) {
	taskID, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	fileID, err2 := strconv.ParseInt(r.PathValue("file"), 10, 64)
	if err1 != nil || err2 != nil {
		http.NotFound(w, r)
		return nil, false
	}
	file, err := s.store.GetAttachment(taskID, fileID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return file, true
}

// markdown converts task text to HTML. goldmark drops raw HTML and unsafe
// links unless told otherwise, so the result is safe to embed.
func (s *server) markdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := s.md.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(buf.String())
}

func (s *server) render(w http.ResponseWriter, code int, name string, data any) {
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "base", data); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	buf.WriteTo(w)
}

func (s *server) fail(w http.ResponseWriter, err error) {
	log.Println("web:", err)
	http.Error(w, "Something went wrong: "+err.Error(), http.StatusInternalServerError)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Format("Jan 2")
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}
