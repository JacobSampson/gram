package tunneledmcp

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	gen "github.com/speakeasy-api/gram/server/gen/tunneled_mcp"
	"github.com/speakeasy-api/gram/server/gen/types"
	"github.com/speakeasy-api/gram/server/internal/authz"
	"github.com/speakeasy-api/gram/server/internal/authztest"
	"github.com/speakeasy-api/gram/server/internal/oops"
	"github.com/speakeasy-api/gram/server/internal/tunneledmcp/repo"
	"github.com/speakeasy-api/gram/server/internal/tunnelmetrics"
)

func TestGetMetricsRequiresSourceProjectAccess(t *testing.T) {
	t.Parallel()
	ctx, ti := newTestService(t)
	authCtx := requireAuthContext(t, ctx)
	server := seedTunneledMcpServer(t, ctx, ti.conn, *authCtx.ProjectID)
	payload := &gen.GetServerMetricsPayload{ID: server.ID.String(), Window: "hour"}
	_, err := ti.service.GetServerMetrics(authztest.WithExactGrants(t, ctx), payload)
	requireOopsCode(t, err, oops.CodeForbidden)
	readCtx := authztest.WithExactGrants(t, ctx, projectScopedMCPGrant(authz.ScopeMCPRead, *authCtx.ProjectID))
	result, err := ti.service.GetServerMetrics(readCtx, payload)
	require.NoError(t, err)
	require.Equal(t, "disabled", result.State)
	payload.ID = uuid.NewString()
	_, err = ti.service.GetServerMetrics(readCtx, payload)
	requireOopsCode(t, err, oops.CodeNotFound)
}
func TestMetricsAlignGaugeOwnersAndPreserveGaps(t *testing.T) {
	t.Parallel()
	since := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	view := &types.TunnelMetrics{}
	rows := []tunnelmetrics.Row{
		{Bucket: since, Kind: "connections", Connections: 2, Consumers: 3},
		{Bucket: since, Kind: "connections", Connections: 4, Consumers: 5},
		{Bucket: since.Add(15 * time.Second), Kind: "connections", Connections: 2, Consumers: 3},
		{Bucket: since, Kind: "requests", Method: "tools/call", Client: "claude", Server: uuid.NewString(), Attempts: 4, Successes: 3, Errors: 1, Bins: []uint64{0, 0, 1, 3, 0, 0, 0, 0, 0, 0, 0, 0}},
	}
	fillMetrics(view, rows, since, since.Add(2*time.Minute), time.Minute)
	require.Len(t, view.Points, 2)
	require.InDelta(t, 4.0, *view.Points[0].Connections, 0.0001)
	require.Equal(t, 2, view.Points[0].CoverageSamples)
	require.EqualValues(t, 4, *view.Points[0].ToolCalls)
	require.EqualValues(t, 250, *view.Points[0].P95Ms)
	require.Nil(t, view.Points[1].ToolCalls)
	require.Nil(t, view.Points[1].Connections)
}

func TestCoverageDistinguishesIdleFromMissingAndMarksLoss(t *testing.T) {
	t.Parallel()
	since := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	view := &types.TunnelMetrics{}
	fillMetrics(view, []tunnelmetrics.Row{{Bucket: since, Kind: "coverage", Incomplete: 3}}, since, since.Add(2*time.Minute), time.Minute)
	require.EqualValues(t, 0, *view.Points[0].ToolCalls)
	require.Equal(t, 1, view.Points[0].RequestCoverageSamples)
	require.True(t, view.Points[0].CollectionPartial)
	require.Zero(t, *view.Points[0].Incomplete, "collector loss is not a request outcome")
	require.Nil(t, view.Points[1].ToolCalls)
}

func TestMetricOwnerLookupOmitsDeletedAndUnknownSources(t *testing.T) {
	t.Parallel()
	ctx, ti := newTestService(t)
	authCtx := requireAuthContext(t, ctx)
	live := seedTunneledMcpServer(t, ctx, ti.conn, *authCtx.ProjectID)
	deleted := seedTunneledMcpServer(t, ctx, ti.conn, *authCtx.ProjectID)
	q := repo.New(ti.conn)
	_, err := q.DeleteServer(ctx, repo.DeleteServerParams{ID: deleted.ID, ProjectID: *authCtx.ProjectID})
	require.NoError(t, err)
	owners, err := q.ListMetricSourceOwners(ctx, []uuid.UUID{live.ID, deleted.ID, uuid.New()})
	require.NoError(t, err)
	require.Equal(t, []repo.ListMetricSourceOwnersRow{{ID: live.ID, ProjectID: *authCtx.ProjectID}}, owners)
}
