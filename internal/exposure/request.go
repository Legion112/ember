// Package exposure decides and then verifies what of a node is reachable from
// the internet.
//
// It exists because of one concrete provider behaviour. On RunPod a port
// declared `8188/http` is served at https://{id}-8188.proxy.runpod.net with no
// authentication of any kind -- no token, no basic auth, nothing -- and the
// workload ember is built to run there is ComfyUI, which has arbitrary file
// read and arbitrary code execution through custom nodes. One wrong field in
// one API body is therefore not a misconfiguration, it is a public shell on a
// machine billed to the operator. So the decision is made in a type that
// cannot spell the dangerous thing (PortRequest), and then the result is read
// back from the provider and checked independently (AssertSSHOnly), because a
// record of what ember asked for cannot catch a provider that published
// something else.
package exposure

import (
	"fmt"

	"github.com/Legion112/ember/internal/node"
)

// PortRequest is one port ember asks a provider to open.
//
// Every field is unexported, so there is no struct-literal path to a public
// port: a PortRequest marked public can only have come from SSHPort. That is
// the difference between a rule written in a comment and a rule the compiler
// enforces. The equivalent code with an exported `Public bool` is one typo in
// one provider away from publishing ComfyUI, and that typo reviews cleanly.
type PortRequest struct {
	port   uint16
	proto  node.Proto
	public bool
}

// SSHPort is the only constructor that produces a publicly reachable request.
//
// It takes no arguments on purpose. A port number parameter would mean a
// caller could pass 8188 and get a public 8188, which is the entire thing this
// package exists to prevent.
func SSHPort() PortRequest {
	return PortRequest{port: 22, proto: node.ProtoTCP, public: true}
}

// Internal asks for a port the node may listen on but nothing outside may
// reach. Workload ports are these: they are reached by forwarding over the SSH
// connection, so they never need to be published at all.
func Internal(port uint16) (PortRequest, error) {
	if port == 0 {
		return PortRequest{}, fmt.Errorf("port 0 is not a port: it is the zero value, and to net.Listen it means \"any free port\"")
	}
	return PortRequest{port: port, proto: node.ProtoTCP}, nil
}

func (r PortRequest) Port() uint16      { return r.port }
func (r PortRequest) Proto() node.Proto { return r.proto }

// IsPublic reports whether ember is asking for this port to be reachable from
// the internet. Only SSHPort can make it true.
func (r PortRequest) IsPublic() bool { return r.public }

func (r PortRequest) String() string {
	vis := "internal"
	if r.public {
		vis = "public"
	}
	return fmt.Sprintf("%d/%s %s", r.port, r.proto, vis)
}

// IsZero reports whether r came from a struct literal rather than a
// constructor. Providers refuse these: a zero request names port 0, and a
// provider that forwards it to an API gets whatever that API does with 0.
func (r PortRequest) IsZero() bool { return r == PortRequest{} }

// Requests is the canonical set ember asks for: sshd, and the workload ports
// as internal-only.
//
// This is the one place the set is built, so "workload ports are never
// published" is a property of this function rather than something each
// provider has to remember. A provider receives the list and maps it; it never
// decides visibility.
func Requests(workload []node.PortMap) ([]PortRequest, error) {
	out := []PortRequest{SSHPort()}
	for _, p := range workload {
		r, err := Internal(p.Remote)
		if err != nil {
			return nil, fmt.Errorf("workload port %q: %w", p.Name, err)
		}
		out = append(out, r)
	}
	return out, nil
}
