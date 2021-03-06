package dht

import (
  //"errors"
  "golang.org/x/net/context"
	"google.golang.org/grpc"
  "fmt"
)

////////
// Finding responsible nodes
////////

// Find closest predecessing finger from the personal table
func (n *RingNode) getClosestPreceding(id uint64) (finger, error) {

	for i := len(n.fingerTable) - 1; i >= 0; i-- {
		if n.inInterval(n.self.ID, id, n.fingerTable[i].ID, false, false) {
      // In case dude has fallen just go to the other one
      _, err := n.invokeGetPred(n.fingerTable[i].IP)
      if err == nil {
        return n.fingerTable[i], nil
      }
		}
	}

  return n.self, nil

}

// Going to reuse this function in findPredecessor and during
// construction of finger table.
func (n *RingNode) recursivePredFindingStep(id uint64, remoteNode finger, currNode finger) finger {

	if remoteNode.ID == currNode.ID {
		return currNode
	}

	next, err := n.invokeFindPred(remoteNode.IP, id)
	if err != nil {
    fmt.Println("Couldn't make a recursive step.")
		panic(err)
	}

	//fmt.Printf("recursive step: id %d remote %s ans %s \n", id, remoteNode.IP, next.IP)

	return n.recursivePredFindingStep(id, next, remoteNode)
}

// Find predecessor in a ring
func (n *RingNode) findPredecessor(id uint64) (finger, error) {

	nextTarget, err := n.getClosestPreceding(id)
	if err != nil {
		return finger{}, err
	}

	// Ask the node for the closest ones in its table recursivly
	return n.recursivePredFindingStep(id, nextTarget, n.self), nil
}

// FindSuccessor finds successor node for a given id
func (n *RingNode) FindSuccessor(id uint64) (string, error) {

	pred, err := n.findPredecessor(id)
	if err != nil {
		panic(err)
	}

	if pred.IP == n.self.IP {
		return n.fingerTable[0].IP, nil
	}

	ans, err := n.invokeGetSucc(pred.IP)
	return ans.IP, err
}

////////
// Remote calls
////////

//// Simple getters

// GetNodeSucc gets successor of a node
func (n *RingNode) GetNodeSucc(ctx context.Context, in *GetNodeSuccRequest) (*NodeReply, error) {
	return &NodeReply{IP: n.fingerTable[0].IP, ID: n.fingerTable[0].ID}, nil
}

func (n *RingNode) invokeGetSucc(IP string) (finger, error) {

	conn, cl := getConn(IP)

	mes, err := cl.GetNodeSucc(
		context.Background(),
		&GetNodeSuccRequest{},
	)
	if err != nil {
		return finger{}, err
	}
	conn.Close()

	return finger{ID: mes.GetID(), IP: mes.GetIP()}, nil
}

// GetNodePred gets the predecessor of a node
func (n *RingNode) GetNodePred(ctx context.Context, in *GetNodePredRequest) (*NodeReply, error) {
	return &NodeReply{IP: n.predecessor.IP, ID: n.predecessor.ID}, nil
}
func (n *RingNode) invokeGetPred(IP string) (finger, error) {

	conn, cl := getConn(IP)

	mes, err := cl.GetNodePred(
		context.Background(),
		&GetNodePredRequest{},
	)
	if err != nil {
		return finger{}, err
	}
	conn.Close()

	return finger{ID: mes.GetID(), IP: mes.GetIP()}, nil
}

//// Recursive finding

// FindPred finds predecessor of certain id
func (n *RingNode) FindPred(ctx context.Context, in *FindPredRequest) (*NodeReply, error) {
	res, err := n.getClosestPreceding(in.ID)
	return &NodeReply{IP: res.IP, ID: res.ID}, err
}

func (n *RingNode) invokeFindPred(invokeIP string, id uint64) (finger, error) {

	conn, err := grpc.Dial(invokeIP, grpc.WithInsecure())
	if err != nil {
		return finger{}, err
	}
	cl := NewRingServiceClient(conn)

	mes, err := cl.FindPred(
		context.Background(),
		&FindPredRequest{ID: id},
	)
	if err != nil {
		return finger{}, err
	}
	conn.Close()

	return finger{ID: mes.GetID(), IP: mes.GetIP()}, nil
}
