package gpu

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"time"

	contract "github.com/hash066/cerberus/contract/go"
	"github.com/hash066/cerberus/daemon/auth"
	"github.com/hash066/cerberus/daemon/mesh"
)

const remoteDispatchTimeout = 15 * time.Second

// DispatchRemote runs a kernel on an explicit worker peer using the signed mesh
// GPU capability pattern (MeshGpuResource + byte/FLOP quota).
func DispatchRemote(
	ctx context.Context,
	fab *mesh.Fabric,
	site string,
	worker contract.PeerID,
	k Kernel,
	param float32,
	a, b []float32,
) (out []float32, backend, where string, err error) {
	if fab == nil {
		return nil, "", "", fmt.Errorf("gpu: mesh not composed")
	}
	req := mesh.GpuRequest{KernelID: int(k), Param: param, A: a, B: b}
	env, issuerID, err := mintGpuCap(fab, site, req)
	if err != nil {
		return nil, "", "", err
	}
	rctx, cancel := context.WithTimeout(ctx, remoteDispatchTimeout)
	defer cancel()
	res, err := fab.RequestGpuSigned(rctx, worker, req, issuerID, env)
	if err != nil {
		return nil, "", "", err
	}
	return res.Output, res.Backend, fmt.Sprintf("%x", worker[:4]), nil
}

func mintGpuCap(fab *mesh.Fabric, site string, req mesh.GpuRequest) ([]byte, contract.PeerID, error) {
	signer, err := meshCapSigner(fab)
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	quota := &contract.Quota{
		Bytes: inputBytes(req) + outputBytes(req),
		Flops: estimateFlops(req),
	}
	res := mesh.MeshGpuResource(site)
	res.Quota = quota
	g, err := auth.NewGrant(
		res,
		[]contract.Right{contract.RightExec},
		nil,
		time.Hour,
	)
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	env, err := signer.Issue(g)
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	issuerID, err := signer.IssuerPeerID()
	if err != nil {
		return nil, contract.PeerID{}, err
	}
	return env, issuerID, nil
}

func meshCapSigner(fab *mesh.Fabric) (*auth.SignedCap, error) {
	identity := fab.Identity()
	if len(identity) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("gpu: mesh identity is not a usable Ed25519 private key (len=%d)", len(identity))
	}
	ks, err := auth.NewMemoryKeyStore(identity.Seed())
	if err != nil {
		return nil, fmt.Errorf("gpu: mesh-cap keystore: %w", err)
	}
	return auth.NewSignedCap(ks), nil
}
