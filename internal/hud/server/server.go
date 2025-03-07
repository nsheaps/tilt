package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tilt-dev/tilt/pkg/apis/core/v1alpha1"

	"github.com/golang/protobuf/jsonpb"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	_ "github.com/gorilla/websocket"
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	jsoniter "github.com/json-iterator/go"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	tiltanalytics "github.com/tilt-dev/tilt/internal/analytics"
	"github.com/tilt-dev/tilt/internal/hud/webview"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/internal/store/tiltfiles"
	"github.com/tilt-dev/tilt/pkg/assets"
	"github.com/tilt-dev/tilt/pkg/model"
	proto_webview "github.com/tilt-dev/tilt/pkg/webview"
	"github.com/tilt-dev/wmclient/pkg/analytics"
)

const TiltTokenCookieName = "Tilt-Token"

// CSRF token to protect the websocket. See:
// https://dev.solita.fi/2018/11/07/securing-websocket-endpoints.html
// https://christian-schneider.net/CrossSiteWebSocketHijacking.html
var websocketCSRFToken = uuid.New()

type analyticsPayload struct {
	Verb string            `json:"verb"`
	Name string            `json:"name"`
	Tags map[string]string `json:"tags"`
}

type analyticsOptPayload struct {
	Opt string `json:"opt"`
}

type triggerPayload struct {
	ManifestNames []string          `json:"manifest_names"`
	BuildReason   model.BuildReason `json:"build_reason"`
}

type overrideTriggerModePayload struct {
	ManifestNames []string `json:"manifest_names"`
	TriggerMode   int      `json:"trigger_mode"`
}

type HeadsUpServer struct {
	ctx        context.Context
	store      *store.Store
	router     *mux.Router
	a          *tiltanalytics.TiltAnalytics
	wsList     *WebsocketList
	ctrlClient ctrlclient.Client
}

func ProvideHeadsUpServer(
	ctx context.Context,
	store *store.Store,
	assetServer assets.Server,
	analytics *tiltanalytics.TiltAnalytics,
	wsList *WebsocketList,
	ctrlClient ctrlclient.Client) (*HeadsUpServer, error) {
	r := mux.NewRouter().UseEncodedPath()
	s := &HeadsUpServer{
		ctx:        ctx,
		store:      store,
		router:     r,
		a:          analytics,
		wsList:     wsList,
		ctrlClient: ctrlClient,
	}

	r.HandleFunc("/api/view", s.ViewJSON)
	r.HandleFunc("/api/dump/engine", s.DumpEngineJSON)
	r.HandleFunc("/api/analytics", s.HandleAnalytics)
	r.HandleFunc("/api/analytics_opt", s.HandleAnalyticsOpt)
	r.HandleFunc("/api/trigger", s.HandleTrigger)
	r.HandleFunc("/api/override/trigger_mode", s.HandleOverrideTriggerMode)
	// this endpoint is only used for testing snapshots in development
	r.HandleFunc("/api/snapshot/{snapshot_id}", s.SnapshotJSON)
	r.HandleFunc("/api/websocket_token", s.WebsocketToken)
	r.HandleFunc("/ws/view", s.ViewWebsocket)
	r.HandleFunc("/api/set_tiltfile_args", s.HandleSetTiltfileArgs).Methods("POST")

	r.PathPrefix("/").Handler(s.cookieWrapper(assetServer))

	return s, nil
}

type funcHandler struct {
	f func(w http.ResponseWriter, r *http.Request)
}

func (fh funcHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fh.f(w, r)
}

func (s *HeadsUpServer) cookieWrapper(handler http.Handler) http.Handler {
	return funcHandler{f: func(w http.ResponseWriter, r *http.Request) {
		state := s.store.RLockState()
		http.SetCookie(w, &http.Cookie{Name: TiltTokenCookieName, Value: string(state.Token), Path: "/"})
		s.store.RUnlockState()
		handler.ServeHTTP(w, r)
	}}
}

func (s *HeadsUpServer) Router() http.Handler {
	return s.router
}

func (s *HeadsUpServer) ViewJSON(w http.ResponseWriter, req *http.Request) {
	view, err := webview.CompleteView(req.Context(), s.ctrlClient, s.store)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error converting view to proto: %v", err), http.StatusInternalServerError)
		return
	}

	jsEncoder := &runtime.JSONPb{}

	w.Header().Set("Content-Type", "application/json")
	err = jsEncoder.NewEncoder(w).Encode(view)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error rendering view payload: %v", err), http.StatusInternalServerError)
	}
}

// Dump the JSON engine over http. Only intended for 'tilt dump engine'.
func (s *HeadsUpServer) DumpEngineJSON(w http.ResponseWriter, req *http.Request) {
	state := s.store.RLockState()
	defer s.store.RUnlockState()

	encoder := store.CreateEngineStateEncoder(w)
	err := encoder.Encode(state)
	if err != nil {
		log.Printf("Error encoding: %v", err)
	}
}

func (s *HeadsUpServer) SnapshotJSON(w http.ResponseWriter, req *http.Request) {
	view, err := webview.CompleteView(req.Context(), s.ctrlClient, s.store)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error converting view to proto: %v", err), http.StatusInternalServerError)
		return
	}

	snapshot := &proto_webview.Snapshot{
		View:      view,
		CreatedAt: timestamppb.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	var m jsonpb.Marshaler
	err = m.Marshal(w, snapshot)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error rendering view payload: %v", err), http.StatusInternalServerError)
	}
}

func (s *HeadsUpServer) HandleAnalyticsOpt(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "must be POST request", http.StatusBadRequest)
		return
	}

	var payload analyticsOptPayload

	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	opt, err := analytics.ParseOpt(payload.Opt)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing opt '%s': %v", payload.Opt, err), http.StatusBadRequest)
	}

	// only logging on opt-in, because, well, opting out means the user just told us not to report data on them!
	if opt == analytics.OptIn {
		s.a.Incr("analytics.opt.in", nil)
	}

	s.store.Dispatch(store.AnalyticsUserOptAction{Opt: opt})
}

func (s *HeadsUpServer) HandleAnalytics(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "must be POST request", http.StatusBadRequest)
		return
	}

	var payloads []analyticsPayload

	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&payloads)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	for _, p := range payloads {
		if p.Verb != "incr" {
			http.Error(w, "error parsing payloads: only incr verbs are supported", http.StatusBadRequest)
			return
		}

		s.a.Incr(p.Name, p.Tags)
	}
}

func (s *HeadsUpServer) HandleSetTiltfileArgs(w http.ResponseWriter, req *http.Request) {
	var args []string
	err := jsoniter.NewDecoder(req.Body).Decode(&args)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	ctx := req.Context()
	err = tiltfiles.SetTiltfileArgs(ctx, s.ctrlClient, args)
	if err != nil {
		http.Error(w, fmt.Sprintf("error updating apiserver: %v", err), http.StatusInternalServerError)
		return
	}
}

// Responds with:
// * 200/empty body on success
// * 200/error message in body on well-formed, unservicable requests (e.g. resource is disabled or doesn't exist)
// * 400/error message in body on badly formed requests (e.g., invalid json)
func (s *HeadsUpServer) HandleTrigger(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "must be POST request", http.StatusBadRequest)
		return
	}

	var payload triggerPayload

	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	if len(payload.ManifestNames) != 1 {
		http.Error(w, fmt.Sprintf("/api/trigger currently supports exactly one manifest name, got %d", len(payload.ManifestNames)), http.StatusBadRequest)
		return
	}

	mn := model.ManifestName(payload.ManifestNames[0])

	state := s.store.RLockState()
	defer s.store.RUnlockState()
	ms, ok := state.ManifestState(mn)
	if !ok {
		http.Error(w, fmt.Sprintf("resource %q does not exist", mn), http.StatusNotFound)
	} else if ms != nil && ms.DisableState == v1alpha1.DisableStateDisabled {
		_, _ = fmt.Fprintf(w, "resource %q is currently disabled", mn)
	} else {
		s.store.Dispatch(AppendToTriggerQueueAction{Name: mn, Reason: payload.BuildReason})
	}
}

func (s *HeadsUpServer) HandleOverrideTriggerMode(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "must be POST request", http.StatusBadRequest)
		return
	}

	var payload overrideTriggerModePayload

	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&payload)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	err = checkManifestsExist(s.store, payload.ManifestNames)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !model.ValidTriggerMode(model.TriggerMode(payload.TriggerMode)) {
		http.Error(w, fmt.Sprintf("invalid trigger mode: %d", payload.TriggerMode), http.StatusBadRequest)
		return
	}

	s.store.Dispatch(OverrideTriggerModeAction{
		ManifestNames: model.ManifestNames(payload.ManifestNames),
		TriggerMode:   model.TriggerMode(payload.TriggerMode),
	})
}

func (s *HeadsUpServer) WebsocketToken(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(websocketCSRFToken.String()))
}

func checkManifestsExist(st store.RStore, mNames []string) error {
	state := st.RLockState()
	defer st.RUnlockState()
	for _, mName := range mNames {
		if _, ok := state.ManifestState(model.ManifestName(mName)); !ok {
			return fmt.Errorf("no manifest found with name '%s'", mName)
		}
	}
	return nil
}
