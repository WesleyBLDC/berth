package berthd

import (
	"encoding/hex"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/core/grant"
	"berth/internal/core/ports"
	"berth/internal/core/secret"
)

var (
	consumerKinds = map[ports.ConsumerKind]berthv1.ConsumerKind{
		ports.ConsumerExe:   berthv1.ConsumerKind_CONSUMER_KIND_EXECUTABLE,
		ports.ConsumerProxy: berthv1.ConsumerKind_CONSUMER_KIND_PROXY,
	}
	signatureKinds = map[ports.Signature]berthv1.SignatureKind{
		ports.SignatureUnsigned: berthv1.SignatureKind_SIGNATURE_KIND_UNSIGNED,
		ports.SignatureAdHoc:    berthv1.SignatureKind_SIGNATURE_KIND_AD_HOC,
		ports.SignatureApple:    berthv1.SignatureKind_SIGNATURE_KIND_APPLE_PLATFORM,
		ports.SignatureTeam:     berthv1.SignatureKind_SIGNATURE_KIND_TEAM,
		ports.SignatureOther:    berthv1.SignatureKind_SIGNATURE_KIND_OTHER,
	}
	ticketActions = map[ports.TicketAction]berthv1.TicketAction{
		ports.TicketUse:    berthv1.TicketAction_TICKET_ACTION_USE,
		ports.TicketRemove: berthv1.TicketAction_TICKET_ACTION_REMOVE,
		ports.TicketRotate: berthv1.TicketAction_TICKET_ACTION_ROTATE,
	}
)

func consumer(g ports.GrantRecord, st grant.State) *berthv1.Consumer {
	kind := kindOf(g)
	c := &berthv1.Consumer{
		GrantId: g.ID, State: grantState(st), Kind: consumerKinds[kind], ExePath: g.Exe.Path,
		TeamId: g.Exe.TeamID, SigningId: g.Exe.SigningID, ArgsGlob: g.ArgsGlob,
		CreatedAt: timestamppb.New(g.CreatedAt), UseCount: g.UseCount, PassEnv: g.PassEnv,
	}
	if kind == ports.ConsumerProxy {
		c.Proxy = proxyTarget(g.Proxy)
	} else {
		c.Sha256 = hex.EncodeToString(g.Exe.SHA256[:])
		c.Signature = signatureKinds[g.Exe.Signature]
	}
	if !g.LastUsedAt.IsZero() {
		c.LastUsedAt = timestamppb.New(g.LastUsedAt)
	}
	return c
}

func secretState(s secret.State) berthv1.SecretState {
	switch s {
	case secret.Pending:
		return berthv1.SecretState_SECRET_STATE_PENDING
	case secret.Stored:
		return berthv1.SecretState_SECRET_STATE_STORED
	case secret.Denied:
		return berthv1.SecretState_SECRET_STATE_DENIED
	case secret.Failed:
		return berthv1.SecretState_SECRET_STATE_FAILED
	case secret.Rotating:
		return berthv1.SecretState_SECRET_STATE_ROTATING
	case secret.Removing:
		return berthv1.SecretState_SECRET_STATE_REMOVING
	case secret.Removed:
		return berthv1.SecretState_SECRET_STATE_REMOVED
	}
	return berthv1.SecretState_SECRET_STATE_UNSPECIFIED
}

func grantState(s grant.State) berthv1.GrantState {
	switch s {
	case grant.Requested:
		return berthv1.GrantState_GRANT_STATE_REQUESTED
	case grant.Active:
		return berthv1.GrantState_GRANT_STATE_ACTIVE
	case grant.Denied:
		return berthv1.GrantState_GRANT_STATE_DENIED
	case grant.Stale:
		return berthv1.GrantState_GRANT_STATE_STALE
	case grant.Revoked:
		return berthv1.GrantState_GRANT_STATE_REVOKED
	}
	return berthv1.GrantState_GRANT_STATE_UNSPECIFIED
}
