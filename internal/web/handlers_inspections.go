package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lan-dot-party/lanventory/internal/app"
	"github.com/lan-dot-party/lanventory/internal/store"
)

// today is the reference date of due states (UTC).
func today() time.Time { return time.Now().UTC().Truncate(24 * time.Hour) }

func validDate(s string) bool {
	_, err := time.Parse(store.DateLayout, s)
	return err == nil
}

// --- record an inspection ---

type inspFormData struct {
	Asset    *store.Asset
	Category *store.Category
	Interval int
	V        map[string]string
	Err      map[string]string
	// ShowDefective offers "set to defective" (active asset), ShowActive "set
	// back to active" (defective asset).
	ShowDefective bool
	ShowActive    bool
	Classes       []string
	LimitPE       float64
	LimitIns1     float64
	LimitIns2     float64
	LimitLeak     float64
	LimitTouch    float64
	Reasons       []string
	Methods       []string
	// CalibExpired shows the calibration hint.
	CalibExpired bool
}

// inspectableAsset loads the asset of the request and refuses assets that
// cannot be inspected (category without DGUV V3, retired or lost).
func (s *Server) inspectableAsset(w http.ResponseWriter, r *http.Request) (*store.Asset, *store.Category, bool) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return nil, nil, false
	}
	a, err := s.DB.AssetByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return nil, nil, false
	}
	cat, err := s.DB.CategoryByID(r.Context(), a.CategoryID)
	if err != nil {
		s.fail(w, r, err)
		return nil, nil, false
	}
	switch {
	case !cat.DGUVRequired:
		s.errorPage(w, r, http.StatusUnprocessableEntity, "error.not_inspectable")
	case a.Status == store.StatusRetired || a.Status == store.StatusLost:
		s.errorPage(w, r, http.StatusUnprocessableEntity, "error.asset_inactive")
	default:
		return a, cat, true
	}
	return nil, nil, false
}

func (s *Server) renderInspForm(w http.ResponseWriter, r *http.Request, status int, a *store.Asset, cat *store.Category,
	v map[string]string, ve *store.ValidationError) {
	p := s.invPage(r, "insp.record", "assets", nil)
	d := inspFormData{Asset: a, Category: cat, V: v, Err: map[string]string{},
		Interval:      store.EffectiveInterval(a.DGUVIntervalMonths, cat.DGUVIntervalMonths),
		ShowDefective: a.Status == store.StatusActive, ShowActive: a.Status == store.StatusDefective,
		Classes: store.ProtectionClasses, LimitPE: store.LimitPEResistanceOhm, LimitIns1: store.LimitInsulationClassI,
		LimitIns2: store.LimitInsulationClassII, LimitLeak: store.LimitLeakageClassI, LimitTouch: store.LimitTouch,
		Reasons: store.InspectionReasons, Methods: store.LeakageMethods,
		CalibExpired: v["test_device_calibrated_until"] != "" && v["inspected_on"] != "" && v["test_device_calibrated_until"] < v["inspected_on"]}
	if ve != nil {
		for field, code := range ve.Fields {
			d.Err[field] = p.T("verr." + code)
		}
		p.Error = p.T("error.form")
	}
	p.Data = d
	s.renderStatus(w, r, status, "inspection_form", p)
}

func (s *Server) inspectionNew(w http.ResponseWriter, r *http.Request) {
	a, cat, ok := s.inspectableAsset(w, r)
	if !ok {
		return
	}
	u := app.User(r.Context())
	v := map[string]string{"inspected_on": today().Format(store.DateLayout), "inspector_name": u.Username, "reason": store.ReasonInitial}
	// A repeat inspection when the asset was inspected before.
	list, err := s.DB.InspectionsByAsset(r.Context(), a.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(list) > 0 {
		v["reason"] = store.ReasonRepeat
	}
	// Convenience: the test device of the user's last inspection.
	switch last, err := s.DB.LastInspectionByUser(r.Context(), u.ID); {
	case err == nil:
		v["test_device"], v["test_device_id"], v["test_device_calibrated_until"] = last.TestDevice, last.TestDeviceID, last.TestDeviceCalibratedUntil
	case !errors.Is(err, store.ErrNotFound):
		s.fail(w, r, err)
		return
	}
	s.renderInspForm(w, r, http.StatusOK, a, cat, v, nil)
}

func (s *Server) inspectionCreate(w http.ResponseWriter, r *http.Request) {
	a, cat, ok := s.inspectableAsset(w, r)
	if !ok {
		return
	}
	get := func(k string) string { return r.PostFormValue(k) }
	v := map[string]string{}
	for _, k := range []string{"inspected_on", "inspector_name", "result", "protection_class", "pe_resistance_ohm",
		"insulation_mohm", "leakage_ma", "touch_ma", "leakage_method", "reason", "test_device", "test_device_id",
		"test_device_calibrated_until", "remark", "visual_ok", "function_ok", "set_defective", "set_active"} {
		v[k] = get(k)
	}
	u := app.User(r.Context())
	in := &store.Inspection{AssetID: a.ID, InspectedOn: v["inspected_on"], InspectorName: v["inspector_name"], RecordedBy: u.ID,
		Result: v["result"], ProtectionClass: v["protection_class"], VisualOK: v["visual_ok"] == "1", FunctionOK: v["function_ok"] == "1",
		PEInput: v["pe_resistance_ohm"], InsulationInput: v["insulation_mohm"], LeakageInput: v["leakage_ma"], TouchInput: v["touch_ma"],
		LeakageMethod: v["leakage_method"], Reason: v["reason"], TestDevice: v["test_device"], TestDeviceID: v["test_device_id"],
		TestDeviceCalibratedUntil: v["test_device_calibrated_until"], Remark: v["remark"]}
	oldSt, newSt, err := s.DB.CreateInspection(r.Context(), in, v["set_defective"] == "1", v["set_active"] == "1")
	switch {
	case errors.Is(err, store.ErrNotInspectable):
		s.errorPage(w, r, http.StatusUnprocessableEntity, "error.not_inspectable")
		return
	case errors.Is(err, store.ErrAssetInactive):
		s.errorPage(w, r, http.StatusUnprocessableEntity, "error.asset_inactive")
		return
	}
	if ve, ok := asValidation(err); ok {
		s.renderInspForm(w, r, http.StatusUnprocessableEntity, a, cat, v, ve)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "inspection.create", "inspection", idStr(in.ID), map[string]any{"asset_id": a.ID, "inventory_no": a.InventoryNo,
		"result": in.Result, "inspected_on": in.InspectedOn, "next_due": in.NextDue})
	if newSt != oldSt {
		s.Audit(r, "asset.update", "asset", idStr(a.ID), map[string]any{"inventory_no": a.InventoryNo, "name": a.Name,
			"via": "inspection", "changes": map[string][2]string{"status": {oldSt, newSt}}})
	}
	redirect(w, r, "/inspections/"+idStr(in.ID)+"?ok=created")
}

// --- detail, protocol, delete ---

type inspDetailData struct {
	In     *store.Inspection
	Asset  *store.Asset
	Fields []fieldValue
	Flags  []store.GuidanceFlag
	// CalibExpired: the calibration of the test device ended before the inspection.
	CalibExpired bool
	// Org is the organisation name (protocol header).
	Org string
}

func (s *Server) loadInspection(w http.ResponseWriter, r *http.Request) (*inspDetailData, bool) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return nil, false
	}
	ctx := r.Context()
	in, err := s.DB.InspectionByID(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	a, err := s.DB.AssetByID(ctx, in.AssetID)
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	fields, err := s.DB.CategoryFields(ctx, a.CategoryID)
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	d := &inspDetailData{In: in, Asset: a, Flags: in.Flags(), CalibExpired: in.TestDeviceCalibratedUntil != "" && in.TestDeviceCalibratedUntil < in.InspectedOn, Org: strings.TrimSpace(s.Cfg.Organization.Name)}
	for _, f := range fields {
		d.Fields = append(d.Fields, fieldValue{Label: f.Label, Unit: f.Unit, Type: f.Type, Value: a.Values[f.ID]})
	}
	return d, true
}

func (s *Server) inspectionDetail(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadInspection(w, r)
	if !ok {
		return
	}
	p := s.invPage(r, "insp.detail", "inspections", d)
	s.render(w, r, "inspection", p)
}

func (s *Server) inspectionPrint(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadInspection(w, r)
	if !ok {
		return
	}
	p := s.page(r, "insp.protocol", "", d)
	s.renderProtocol(w, p)
}

func (s *Server) inspectionDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	in, err := s.DB.DeleteInspection(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "inspection.delete", "inspection", idStr(id), map[string]any{"asset_id": in.AssetID, "inventory_no": in.InventoryNo,
		"result": in.Result, "inspected_on": in.InspectedOn, "next_due": in.NextDue})
	redirect(w, r, "/assets/"+idStr(in.AssetID)+"?ok=deleted")
}

// --- due overview ---

type dueData struct {
	CategoryID int64
	LocationID int64
	State      string
	States     []string
	Categories []*store.Category
	Locations  []*store.Location
	Rows       []*store.DueRow
	Filtered   bool
}

func (s *Server) inspectionsDue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := &dueData{CategoryID: parseID(q.Get("category")), LocationID: parseID(q.Get("location")),
		States: []string{store.DueOverdue, store.DueSoon, store.DueNever}}
	for _, st := range d.States {
		if q.Get("state") == st {
			d.State = st
		}
	}
	cats, err := s.DB.ListCategories(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, c := range cats {
		if c.DGUVRequired {
			d.Categories = append(d.Categories, c)
		}
	}
	if d.Locations, err = s.DB.ListLocations(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Rows, err = s.DB.ListDue(ctx, store.DueFilter{CategoryID: d.CategoryID, LocationID: d.LocationID, State: d.State, Today: today()}); err != nil {
		s.fail(w, r, err)
		return
	}
	d.Filtered = d.CategoryID != 0 || d.LocationID != 0 || d.State != ""
	p := s.invPage(r, "insp.due_title", "due", d)
	p.HTMX = true
	w.Header().Add("Vary", "HX-Request")
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" {
		s.renderFragment(w, r, "inspections_due", "due_results", p)
		return
	}
	s.render(w, r, "inspections_due", p)
}

// --- list and CSV ---

// inspFilterFromQuery reads the list/CSV filters; invalid values are ignored.
func inspFilterFromQuery(q url.Values) (store.InspectionFilter, url.Values) {
	f := store.InspectionFilter{CategoryID: parseID(q.Get("category")), LocationID: parseID(q.Get("location"))}
	keep := url.Values{}
	if v := q.Get("from"); validDate(v) {
		f.From = v
		keep.Set("from", v)
	}
	if v := q.Get("to"); validDate(v) {
		f.To = v
		keep.Set("to", v)
	}
	if v := q.Get("result"); v == store.ResultPass || v == store.ResultFail {
		f.Result = v
		keep.Set("result", v)
	}
	if f.CategoryID != 0 {
		keep.Set("category", idStr(f.CategoryID))
	}
	if f.LocationID != 0 {
		keep.Set("location", idStr(f.LocationID))
	}
	if v := strings.TrimSpace(q.Get("inspector")); v != "" {
		if r := []rune(v); len(r) > 100 {
			v = string(r[:100])
		}
		f.Inspector = v
		keep.Set("inspector", v)
	}
	return f, keep
}

type inspListData struct {
	F          store.InspectionFilter
	Categories []*store.Category
	Locations  []*store.Location
	Result     *store.InspectionPage
	CSVURL     string
	PrevURL    string
	NextURL    string
	Filtered   bool
}

func (s *Server) inspections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, keep := inspFilterFromQuery(r.URL.Query())
	f.Page = parsePage(r.URL.Query().Get("page"))
	d := &inspListData{F: f, Filtered: len(keep) > 0}
	var err error
	if d.Categories, err = s.DB.ListCategories(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Locations, err = s.DB.ListLocations(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Result, err = s.DB.ListInspections(ctx, f); err != nil {
		s.fail(w, r, err)
		return
	}
	link := func(base string, v url.Values) string {
		if len(v) == 0 {
			return base
		}
		return base + "?" + v.Encode()
	}
	d.CSVURL = link("/inspections.csv", keep)
	withPage := func(n int) string {
		v := cloneValues(keep)
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		return link("/inspections", v)
	}
	if d.Result.Page > 1 {
		d.PrevURL = withPage(d.Result.Page - 1)
	}
	if d.Result.Page < d.Result.Pages {
		d.NextURL = withPage(d.Result.Page + 1)
	}
	s.render(w, r, "inspections", s.invPage(r, "insp.list_title", "inspections", d))
}

// csvHeader lists the columns of the inspection export.
var csvHeader = []string{"inspection_id", "inventory_no", "asset_name", "category", "location", "inspected_on", "result",
	"protection_class", "visual_ok", "function_ok", "pe_resistance_ohm", "pe_resistance_cmp", "insulation_mohm", "insulation_cmp",
	"leakage_ma", "leakage_cmp", "leakage_method", "touch_ma", "touch_cmp", "reason", "test_device", "test_device_id",
	"test_device_calibrated_until", "inspector_name", "next_due", "remark", "created_at"}

func (s *Server) inspectionsCSV(w http.ResponseWriter, r *http.Request) {
	f, _ := inspFilterFromQuery(r.URL.Query())
	cw := newCSVWriter(w, "lanventory-inspections-"+today().Format(store.DateLayout)+".csv")
	_ = cw.Write(csvHeader) // errors surface through Finish below
	err := s.DB.ExportInspections(r.Context(), f, func(in *store.Inspection) error {
		row := []string{idStr(in.ID), in.InventoryNo, in.AssetName, in.CategoryName, in.LocationName, in.InspectedOn, in.Result,
			in.ProtectionClass, csvBool(in.VisualOK), csvBool(in.FunctionOK), csvNum(in.PEResistanceOhm), in.PECmp, csvNum(in.InsulationMOhm),
			in.InsulationCmp, csvNum(in.LeakageMA), in.LeakageCmp, in.LeakageMethod, csvNum(in.TouchMA), in.TouchCmp, in.Reason,
			in.TestDevice, in.TestDeviceID, in.TestDeviceCalibratedUntil, in.InspectorName, in.NextDue, in.Remark, in.CreatedAt.UTC().Format(time.RFC3339)}
		return cw.Write(row)
	})
	if werr := cw.Finish(); err != nil || werr != nil {
		// The status line is already sent; log and end the stream.
		s.Log.Error("inspection export", "err", err, "write", werr)
	}
}

// --- asset detail card ---

type dguvCard struct {
	Required bool
	// State is ok, soon, overdue, never or not_required.
	State   string
	NextDue string
	Last    *store.Inspection
	List    []*store.Inspection
	// CanRecord: the user may inspect and the asset can be inspected.
	CanRecord bool
	// Inactive: inspection required but the asset is retired or lost.
	Inactive bool
}

func (s *Server) dguvCard(r *http.Request, a *store.Asset, cat *store.Category) (*dguvCard, error) {
	list, err := s.DB.InspectionsByAsset(r.Context(), a.ID)
	if err != nil {
		return nil, err
	}
	c := &dguvCard{Required: cat.DGUVRequired, List: list, State: "not_required"}
	if len(list) > 0 {
		c.Last = list[0]
		c.NextDue = list[0].NextDue
	}
	if !cat.DGUVRequired {
		return c, nil
	}
	c.State = store.DueState(c.NextDue, today())
	c.Inactive = a.Status == store.StatusRetired || a.Status == store.StatusLost
	c.CanRecord = !c.Inactive && app.User(r.Context()).CanInspect
	return c, nil
}
