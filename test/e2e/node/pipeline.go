package node

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/dataplane"
	"github.com/hash066/cerberus/daemon/inference"
	"github.com/hash066/cerberus/daemon/scheduler"
	"github.com/hash066/cerberus/daemon/system"
)

var errPipelineNotEnabled = errors.New("pipeline mode not enabled")

type pipelineRunRequest struct {
	Model   string `json:"model"`
	Backend string `json:"backend"`
}

// wirePipeline installs the data plane, activation grants, and a requester-side
// PipelineRunner on an e2e demo node.
func (s *server) wirePipeline(ctx context.Context) error {
	dp := dataplane.NewServer(s.kernel, time.Now().Unix(), s.fabric.Identity())
	if err := dp.Listen("127.0.0.1:0"); err != nil {
		return err
	}
	actRouter := system.NewActivationRouter()
	go func() {
		_ = dp.Serve(ctx, actRouter.Route)
	}()

	worker, err := system.RegisterPipelineWorker(
		s.fabric, s.kernel, dp, actRouter.Register, meshSite, s.resolveIssuerKey, false)
	if err != nil {
		return err
	}
	s.pipelineWorker = worker

	sched := scheduler.New(nil)
	sched.UpdateNode(localNodeTelemetry(s.fabric.PeerID()))

	trusted := map[contract.PeerID][]byte{}
	for id, pub := range s.trustedKeys {
		trusted[id] = append([]byte(nil), pub...)
	}
	runner, err := system.NewPipelineRunnerFromFabric(s.fabric, sched, trusted)
	if err != nil {
		return err
	}
	runner.Signer = s.signer
	runner.Site = meshSite
	s.pipelineRunner = runner
	s.inferenceSvc = system.NewInferenceService(runner, system.BuiltinInferenceModels())
	return nil
}

func localNodeTelemetry(id contract.PeerID) contract.NodeTelemetry {
	return contract.NodeTelemetry{
		PeerID:  id,
		Compute: contract.Compute{PCores: 4, Flops: 5e11},
		Memory:  contract.Memory{RAMTotal: 8_000_000_000, RAMFree: 4_000_000_000, VRAMTotal: 4_000_000_000, VRAMFree: 3_000_000_000},
		Thermal: contract.Thermal{HeadroomC: 25},
		Power:   contract.Power{Src: contract.PowerAC},
	}
}

func (s *server) isPipelineTask(task contract.ComputeTask) bool {
	// ShardPipeline is the zero ShardKind, so Kind alone cannot discriminate:
	// a real pipeline stage always carries a layer range or a pipeline component.
	if task.Shard.Kind == contract.ShardPipeline && task.Shard.LayerHi > task.Shard.LayerLo {
		return true
	}
	return inference.IsPipelineComponent(task.Component)
}

func (s *server) handlePipelineRun(w http.ResponseWriter, r *http.Request) {
	if s.inferenceSvc == nil {
		writeError(w, http.StatusPreconditionFailed, errPipelineNotEnabled)
		return
	}
	s.pipelineRunner.Sched.UpdateNode(localNodeTelemetry(s.fabric.PeerID()))
	for _, p := range s.peersResponse().Peers {
		if p.MeshPeerID == "" || p.IssuerPub == "" {
			continue
		}
		s.connectMesh(r.Context(), p.MeshAddrs)
		pid, err := decodePeerID(p.MeshPeerID)
		if err != nil {
			continue
		}
		s.pipelineRunner.Sched.UpdateNode(localNodeTelemetry(pid))
		raw, err := base64.StdEncoding.DecodeString(p.IssuerPub)
		if err == nil && len(raw) == 32 {
			var id contract.PeerID
			copy(id[:], raw)
			s.trustedKeys[id] = append([]byte(nil), raw...)
		}
	}

	modelID := strings.TrimSpace(r.URL.Query().Get("model"))
	backend := strings.TrimSpace(r.URL.Query().Get("backend"))
	if r.Body != nil {
		var body pipelineRunRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
		if dec.Decode(&body) == nil {
			if modelID == "" {
				modelID = strings.TrimSpace(body.Model)
			}
			if backend == "" {
				backend = strings.TrimSpace(body.Backend)
			}
		}
	}
	if modelID == "" {
		modelID = system.SplitMLPDemoModel.ID
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	subject := fmt.Sprintf("e2e-%s", s.self.ID)
	result, err := s.inferenceSvc.RunWithBackend(ctx, subject, modelID, backend, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	stages := make([]string, 0, len(result.Stages))
	for _, st := range result.Stages {
		stages = append(stages, fmt.Sprintf("%s layers %d-%d",
			hex.EncodeToString(st.Node[:4]), st.Shard.LayerLo, st.Shard.LayerHi))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": result.OK, "output": result.Output, "content": result.Content,
		"error": result.Error, "model": modelID, "backend": result.Backend, "stages": stages,
	})
}

func (s *server) pipelineComputeDispatch(ctx context.Context, task contract.ComputeTask, grant auth.Grant) (contract.ComputeResult, error) {
	if s.pipelineWorker == nil {
		return contract.ComputeResult{TaskID: task.TaskID, OK: false, Error: "pipeline worker not wired"}, nil
	}
	return s.pipelineWorker.HandleCompute(ctx, task, grant)
}
