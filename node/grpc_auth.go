package main

import (
	"crypto/ed25519"
	"fmt"

	"dylaris-pkg/nodeauth"
	pb "dylaris-proto/node"
)

// recvAuthResult reads from the auth stream until the Core delivers an
// AuthResult, transparently answering a challenge nonce along the way. Core sends
// a NodeChallenge before the AuthResult when it wants proof; the answer is fresh
// per nonce, so a captured one cannot be replayed.
//
// The node answers with EVERY proof it holds - the HMAC of its secret and the
// signature of its key - because it cannot know which one this Core reads: a
// Core that predates keys reads only the HMAC, a current one reads the signature
// for a row that holds a key. identity is the node_token this node sent in the
// same NodeAuth, which the signature binds. secret and key are each nil when
// the node lacks them; a challenge arriving with neither is a hard error.
func recvAuthResult(stream pb.NodeService_NodeConnectClient, identity string, secret []byte, key ed25519.PrivateKey) (*pb.AuthResult, error) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		if ch := msg.GetChallenge(); ch != nil {
			if secret == nil && key == nil {
				return nil, fmt.Errorf("core sent a challenge but node has neither a secret nor a key")
			}
			resp := &pb.NodeChallengeResponse{}
			if secret != nil {
				resp.Response = aclChallengeResponse(secret, ch.Nonce)
			}
			if key != nil {
				resp.Signature = nodeauth.SignChallenge(key, identity, ch.Nonce)
			}
			if serr := stream.Send(&pb.NodeMessage{
				Payload: &pb.NodeMessage_ChallengeResponse{ChallengeResponse: resp},
			}); serr != nil {
				return nil, fmt.Errorf("send challenge response: %w", serr)
			}
			continue
		}
		if ar := msg.GetAuthResult(); ar != nil {
			return ar, nil
		}
		return nil, fmt.Errorf("unexpected message during auth (want challenge or auth_result)")
	}
}
