package web

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lan-dot-party/lanventory/internal/store"
)

// --- dashboard ---

type statusCount struct {
	Status string
	N      int
}

type dashboardData struct {
	Users      int
	Total      int
	Statuses   []statusCount
	Categories []*store.Category
	DGUV       store.DueCounts
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	n, err := s.DB.CountUsers(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	counts, err := s.DB.CountAssetsByStatus(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cats, err := s.DB.ListCategories(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := dashboardData{Users: n, Categories: cats}
	if d.DGUV, err = s.DB.CountDue(ctx, today()); err != nil {
		s.fail(w, r, err)
		return
	}
	for _, st := range store.Statuses {
		d.Statuses = append(d.Statuses, statusCount{st, counts[st]})
		d.Total += counts[st]
	}
	s.render(w, r, "dashboard", s.page(r, "dash.title", "dashboard", d))
}

// --- list ---

type sortCol struct {
	Key    string
	URL    string
	Active bool
	Desc   bool
}

type assetListData struct {
	Q          string
	CategoryID int64
	LocationID int64
	Status     string
	Sort       string
	Desc       bool
	Categories []*store.Category
	Locations  []*store.Location
	Statuses   []string
	// Fields are the select and bool fields of the chosen category, offered as filters.
	Fields    []store.Field
	FieldVals map[string]string
	Result    *store.AssetPage
	Sorts     []sortCol
	PrevURL   string
	NextURL   string
	// Filtered is set when any filter or search is active.
	Filtered bool
	// CSVURL is the export link with the current filters and sort.
	CSVURL string
	// BulkFilter is the filter as a query string (no sort, no page) for "all matches" in a bulk edit.
	BulkFilter string
	// BulkMax is the most assets one bulk edit may touch.
	BulkMax int
}

// parsePage parses a 1-based page number; anything invalid or beyond 32 bits is 0.
func parsePage(v string) int {
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		return 0
	}
	return int(n)
}

func parseID(v string) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// assetQuery is the filter state of the asset list and the CSV export.
type assetQuery struct {
	Filter store.AssetFilter
	// Fields are the select and bool fields of the chosen category, offered as filters.
	Fields    []store.Field
	FieldVals map[string]string
}

// parseAssetQuery reads q, category, location, status, f_<key>, sort and dir
// from the URL; invalid values are ignored. The page is not set.
func (s *Server) parseAssetQuery(r *http.Request) (*assetQuery, error) {
	return s.parseAssetValues(r, r.URL.Query())
}

// parseAssetValues is parseAssetQuery for an already parsed query string.
func (s *Server) parseAssetValues(r *http.Request, q url.Values) (*assetQuery, error) {
	aq := &assetQuery{FieldVals: map[string]string{}}
	f := &aq.Filter
	f.Q, f.CategoryID, f.LocationID = strings.TrimSpace(q.Get("q")), parseID(q.Get("category")), parseID(q.Get("location"))
	if st := q.Get("status"); store.ValidStatus(st) {
		f.Status = st
	}
	f.Sort = "inventory_no"
	for _, k := range store.AssetSorts {
		if q.Get("sort") == k {
			f.Sort = k
		}
	}
	f.Desc = q.Get("dir") == "desc"
	f.Fields = map[string]string{}
	if f.CategoryID != 0 {
		fields, err := s.DB.CategoryFields(r.Context(), f.CategoryID)
		if err != nil {
			return nil, err
		}
		for _, fd := range fields {
			if fd.Type != store.FieldSelect && fd.Type != store.FieldBool {
				continue
			}
			aq.Fields = append(aq.Fields, fd)
			if v := q.Get("f_" + fd.Key); v != "" {
				aq.FieldVals[fd.Key] = v
				f.Fields[fd.Key] = v
			}
		}
	}
	return aq, nil
}

func (s *Server) assets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	aq, err := s.parseAssetQuery(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f := aq.Filter
	f.Page = parsePage(q.Get("page"))
	d := &assetListData{Q: f.Q, CategoryID: f.CategoryID, LocationID: f.LocationID, Status: f.Status, Sort: f.Sort, Desc: f.Desc,
		Statuses: store.Statuses, Fields: aq.Fields, FieldVals: aq.FieldVals}
	if d.Categories, err = s.DB.ListCategories(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Locations, err = s.DB.ListLocations(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Result, err = s.DB.ListAssets(ctx, f); err != nil {
		s.fail(w, r, err)
		return
	}
	d.Filtered = d.Q != "" || d.CategoryID != 0 || d.LocationID != 0 || d.Status != "" || len(d.FieldVals) > 0

	// Links keep the whole filter state.
	base := url.Values{}
	if d.Q != "" {
		base.Set("q", d.Q)
	}
	if d.CategoryID != 0 {
		base.Set("category", idStr(d.CategoryID))
	}
	if d.LocationID != 0 {
		base.Set("location", idStr(d.LocationID))
	}
	if d.Status != "" {
		base.Set("status", d.Status)
	}
	for k, v := range d.FieldVals {
		base.Set("f_"+k, v)
	}
	link := func(v url.Values) string {
		if len(v) == 0 {
			return "/assets"
		}
		return "/assets?" + v.Encode()
	}
	csv := cloneValues(base)
	if d.Sort != "inventory_no" || d.Desc {
		csv.Set("sort", d.Sort)
	}
	if d.Desc {
		csv.Set("dir", "desc")
	}
	d.BulkFilter, d.BulkMax = base.Encode(), store.MaxBulkAssets
	d.CSVURL = "/assets.csv"
	if len(csv) > 0 {
		d.CSVURL += "?" + csv.Encode()
	}
	for _, k := range store.AssetSorts {
		v := cloneValues(base)
		v.Set("sort", k)
		active := d.Sort == k
		if active && !d.Desc {
			v.Set("dir", "desc")
		}
		d.Sorts = append(d.Sorts, sortCol{Key: k, URL: link(v), Active: active, Desc: active && d.Desc})
	}
	withPage := func(n int) string {
		v := cloneValues(base)
		if d.Sort != "inventory_no" || d.Desc {
			v.Set("sort", d.Sort)
		}
		if d.Desc {
			v.Set("dir", "desc")
		}
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		return link(v)
	}
	if d.Result.Page > 1 {
		d.PrevURL = withPage(d.Result.Page - 1)
	}
	if d.Result.Page < d.Result.Pages {
		d.NextURL = withPage(d.Result.Page + 1)
	}

	p := s.invPage(r, "assets.title", "assets", d)
	p.HTMX = true
	// htmx requests get only the results; a history restore needs the full page.
	w.Header().Add("Vary", "HX-Request")
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" {
		s.renderFragment(w, r, "assets", "asset_fragment", p)
		return
	}
	s.render(w, r, "assets", p)
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

// --- detail and history ---

type fieldValue struct {
	Label string
	Unit  string
	Type  string
	Value string
}

type changeRow struct {
	Label  string
	Old    string
	New    string
	Status bool
}

type historyEntry struct {
	TS      time.Time
	User    string
	Action  string
	Changes []changeRow
}

type assetDetailData struct {
	Asset    *store.Asset
	Category *store.Category
	Values   []fieldValue
	History  []historyEntry
	DGUV     *dguvCard
}

func (s *Server) assetDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	ctx := r.Context()
	a, err := s.DB.AssetByID(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cat, err := s.DB.CategoryByID(ctx, a.CategoryID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fields, err := s.DB.CategoryFields(ctx, a.CategoryID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	entries, err := s.DB.AuditByObject(ctx, "asset", idStr(id), 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := assetDetailData{Asset: a, Category: cat}
	if d.DGUV, err = s.dguvCard(r, a, cat); err != nil {
		s.fail(w, r, err)
		return
	}
	for _, f := range fields {
		d.Values = append(d.Values, fieldValue{Label: f.Label, Unit: f.Unit, Type: f.Type, Value: a.Values[f.ID]})
	}
	p := s.invPage(r, "assets.title", "assets", nil)
	d.History = s.buildHistory(p, entries, fields)
	p.Data = d
	p.Title = a.InventoryNo
	s.render(w, r, "asset", p)
}

// buildHistory turns audit entries into rows for the detail page.
func (s *Server) buildHistory(p *Page, entries []store.AuditEntry, fields []store.Field) []historyEntry {
	byKey := map[string]store.Field{}
	for _, f := range fields {
		byKey[f.Key] = f
	}
	var out []historyEntry
	for _, e := range entries {
		if e.Action != "asset.create" && e.Action != "asset.update" {
			continue
		}
		h := historyEntry{TS: e.TS, User: e.Actor(), Action: e.Action}
		raw, _ := e.Details["changes"].(map[string]any)
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			pair, _ := raw[k].([]any)
			if len(pair) != 2 {
				continue
			}
			o, _ := pair[0].(string)
			n, _ := pair[1].(string)
			row := changeRow{Old: o, New: n}
			switch {
			case strings.HasPrefix(k, "field:"):
				key := strings.TrimPrefix(k, "field:")
				if f, ok := byKey[key]; ok {
					row.Label = f.Label
					switch f.Type {
					case store.FieldBool:
						row.Old, row.New = p.boolText(o), p.boolText(n)
					case store.FieldNumber:
						row.Old, row.New = fmtNum(p.Lang, o), fmtNum(p.Lang, n)
					}
				} else {
					row.Label = key
				}
			case k == "status":
				row.Status = true
				row.Label = p.T("acol.status")
			default:
				row.Label = p.T("acol." + k)
			}
			h.Changes = append(h.Changes, row)
		}
		out = append(out, h)
	}
	return out
}

// boolText renders a stored bool value.
func (p *Page) boolText(v string) string {
	switch v {
	case "1":
		return p.T("yes")
	case "0":
		return p.T("no")
	}
	return v
}

// --- form ---

type assetFormData struct {
	Asset     *store.Asset // nil when creating
	Category  *store.Category
	Fields    []store.Field
	V         map[string]string
	Err       map[string]string
	Locations []*store.Location
	Statuses  []string
	// Editing only: all categories for the category switch, and – while
	// moving the asset to Category – the values that will be lost.
	Categories []*store.Category
	Moving     bool
	Lost       []lostValue
}

// lostValue is a custom value that does not fit the new category.
type lostValue struct{ Label, Value, Unit, Type string }

func (s *Server) renderAssetForm(w http.ResponseWriter, r *http.Request, status int, a *store.Asset, cat *store.Category,
	fields []store.Field, v map[string]string, lost []lostValue, ve *store.ValidationError) {
	locs, err := s.DB.ListLocations(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	titleKey := "assets.new"
	if a != nil {
		titleKey = "assets.edit"
	}
	p := s.invPage(r, titleKey, "assets", nil)
	d := assetFormData{Asset: a, Category: cat, Fields: fields, V: v, Err: map[string]string{}, Locations: locs, Statuses: store.Statuses}
	if ve != nil {
		for field, code := range ve.Fields {
			d.Err[field] = p.T("verr." + code)
		}
		p.Error = p.T("error.form")
	}
	if a != nil {
		if d.Categories, err = s.DB.ListCategories(r.Context()); err != nil {
			s.fail(w, r, err)
			return
		}
		d.Moving, d.Lost = cat.ID != a.CategoryID, lost
	}
	p.Data = d
	s.renderStatus(w, r, status, "asset_form", p)
}

// targetCategory returns the category an edited asset is shown or saved
// with: the one named by the "category" parameter when it is another,
// existing category, otherwise the asset's own. For a move, the asset's
// custom values are carried over to the new fields (see store.CarryValues)
// and the lost ones are returned for display.
func (s *Server) targetCategory(r *http.Request, a *store.Asset, raw string) (*store.Category, []store.Field, []lostValue, error) {
	ctx := r.Context()
	oldFields, err := s.DB.CategoryFields(ctx, a.CategoryID)
	if err != nil {
		return nil, nil, nil, err
	}
	id := parseID(raw)
	if id == 0 || id == a.CategoryID {
		cat, err := s.DB.CategoryByID(ctx, a.CategoryID)
		return cat, oldFields, nil, err
	}
	cat, err := s.DB.CategoryByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		cat, err := s.DB.CategoryByID(ctx, a.CategoryID)
		return cat, oldFields, nil, err
	}
	if err != nil {
		return nil, nil, nil, err
	}
	fields, err := s.DB.CategoryFields(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	vals, lost := store.CarryValues(a, oldFields, fields)
	out := make([]lostValue, 0, len(lost))
	for _, f := range lost {
		out = append(out, lostValue{f.Label, a.Values[f.ID], f.Unit, f.Type})
	}
	a.Values = vals
	return cat, fields, out, nil
}

// formValues extracts the posted asset form into an Asset (custom values by
// field ID) and a map for redisplaying the input.
func formValues(r *http.Request, fields []store.Field) (*store.Asset, map[string]string) {
	v := map[string]string{}
	get := func(name string) string {
		vals := r.PostForm[name]
		if len(vals) == 0 {
			return ""
		}
		v[name] = vals[len(vals)-1] // a checkbox follows its hidden "0"
		return v[name]
	}
	a := &store.Asset{Name: get("name"), SerialNo: get("serial_no"), Manufacturer: get("manufacturer"), Model: get("model"),
		Status: get("status"), Note: get("note"), PurchaseDate: get("purchase_date"), InventoryNo: get("inventory_no"),
		Values: map[int64]string{}}
	if x := strings.TrimSpace(get("location")); x != "" {
		a.LocationID = -1
		if n, err := strconv.ParseInt(x, 10, 64); err == nil && n > 0 {
			a.LocationID = n
		}
	}
	if x := strings.TrimSpace(get("dguv_interval")); x != "" {
		a.DGUVIntervalMonths = -1
		if n, err := strconv.Atoi(x); err == nil && n > 0 {
			a.DGUVIntervalMonths = n
		}
	}
	for _, f := range fields {
		a.Values[f.ID] = get("f_" + f.Key)
	}
	return a, v
}

// valuesOf fills the form map from a stored asset.
func valuesOf(a *store.Asset, fields []store.Field) map[string]string {
	v := map[string]string{"name": a.Name, "serial_no": a.SerialNo, "manufacturer": a.Manufacturer, "model": a.Model,
		"status": a.Status, "note": a.Note, "purchase_date": a.PurchaseDate, "inventory_no": a.InventoryNo}
	if a.LocationID != 0 {
		v["location"] = idStr(a.LocationID)
	}
	if a.DGUVIntervalMonths != 0 {
		v["dguv_interval"] = strconv.Itoa(a.DGUVIntervalMonths)
	}
	for _, f := range fields {
		v["f_"+f.Key] = a.Values[f.ID]
	}
	return v
}

type assetPickData struct {
	Categories []*store.Category
}

func (s *Server) assetNew(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if cid := parseID(r.URL.Query().Get("category")); cid != 0 {
		cat, err := s.DB.CategoryByID(ctx, cid)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		fields, err := s.DB.CategoryFields(ctx, cid)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		v := map[string]string{"status": store.StatusActive}
		for _, f := range fields {
			if f.Type == store.FieldBool {
				v["f_"+f.Key] = "0"
			}
		}
		s.renderAssetForm(w, r, http.StatusOK, nil, cat, fields, v, nil, nil)
		return
	}
	cats, err := s.DB.ListCategories(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "asset_new", s.invPage(r, "assets.new", "assets", assetPickData{Categories: cats}))
}

func (s *Server) assetCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cat, err := s.DB.CategoryByID(ctx, parseID(r.PostFormValue("category")))
	if err != nil {
		redirect(w, r, "/assets/new")
		return
	}
	fields, err := s.DB.CategoryFields(ctx, cat.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	a, v := formValues(r, fields)
	a.CategoryID = cat.ID
	changes, err := s.DB.CreateAsset(ctx, a, s.Cfg.Inventory.NumberFormat)
	if ve, ok := asValidation(err); ok {
		s.renderAssetForm(w, r, http.StatusUnprocessableEntity, nil, cat, fields, v, nil, ve)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "asset.create", "asset", idStr(a.ID), map[string]any{"inventory_no": a.InventoryNo, "name": a.Name, "changes": changes})
	redirect(w, r, "/assets/"+idStr(a.ID)+"?ok=created")
}

func (s *Server) assetEdit(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	ctx := r.Context()
	a, err := s.DB.AssetByID(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	orig := *a
	cat, fields, lost, err := s.targetCategory(r, a, r.URL.Query().Get("category"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderAssetForm(w, r, http.StatusOK, &orig, cat, fields, valuesOf(a, fields), lost, nil)
}

func (s *Server) assetUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	ctx := r.Context()
	cur, err := s.DB.AssetByID(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	_ = r.ParseForm()
	moved := *cur
	cat, fields, lost, err := s.targetCategory(r, &moved, r.PostFormValue("category"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	a, v := formValues(r, fields)
	a.ID, a.CategoryID = id, cat.ID
	changes, err := s.DB.UpdateAsset(ctx, a)
	if ve, ok := asValidation(err); ok {
		v["inventory_no"] = cur.InventoryNo
		s.renderAssetForm(w, r, http.StatusUnprocessableEntity, cur, cat, fields, v, lost, ve)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(changes) > 0 {
		s.Audit(r, "asset.update", "asset", idStr(id), map[string]any{"inventory_no": cur.InventoryNo, "name": a.Name, "changes": changes})
	}
	redirect(w, r, "/assets/"+idStr(id)+"?ok=saved")
}

func (s *Server) assetDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	a, err := s.DB.AssetByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.DB.DeleteAsset(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "asset.delete", "asset", idStr(id), map[string]any{"inventory_no": a.InventoryNo, "name": a.Name})
	redirect(w, r, "/assets?ok=deleted")
}
