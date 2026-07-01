package gateway

import (
	"encoding/json"
	"net/http"
)

// modelObject is one entry of the OpenAI /v1/models list. The extra
// component_cid field is a Cerberus addition (ignored by vanilla OpenAI clients)
// that tells a caller which WASM component/agent backs the model.
type modelObject struct {
	ID           string `json:"id"`
	Object       string `json:"object"`
	Created      int64  `json:"created"`
	OwnedBy      string `json:"owned_by"`
	ComponentCID string `json:"component_cid,omitempty"`
}

// modelList is the OpenAI list envelope.
type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// HandleModels serves GET /v1/models — the components/agents available as models.
// It is Bearer-cap-gated like every other route (an "exec" token): listing what
// you can run is itself a capability, not public metadata.
func (g *Gateway) HandleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if _, ok := g.authorize(w, r); !ok {
		return
	}

	models := g.listModels()
	data := make([]modelObject, 0, len(models))
	for _, m := range models {
		data = append(data, modelObject{
			ID:           m.ID,
			Object:       "model",
			Created:      m.Created,
			OwnedBy:      m.OwnedBy,
			ComponentCID: m.ComponentCID,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelList{Object: "list", Data: data})
}
