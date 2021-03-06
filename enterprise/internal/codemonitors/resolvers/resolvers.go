package resolvers

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/relay"
	"github.com/keegancsmith/sqlf"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/backend"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend/graphqlutil"
	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/db/basestore"
	"github.com/sourcegraph/sourcegraph/internal/db/dbutil"
)

// NewResolver returns a new Resolver that uses the given db
func NewResolver(db dbutil.DB) graphqlbackend.CodeMonitorsResolver {
	return &Resolver{db: basestore.NewWithDB(db, sql.TxOptions{}), clock: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }}
}

// newResolverWithClock is used in tests to set the clock manually.
func newResolverWithClock(db dbutil.DB, clock func() time.Time) graphqlbackend.CodeMonitorsResolver {
	return &Resolver{db: basestore.NewWithDB(db, sql.TxOptions{}), clock: clock}
}

type Resolver struct {
	db    *basestore.Store
	clock func() time.Time
}

func (r *Resolver) Monitors(ctx context.Context, userID int32, args *graphqlbackend.ListMonitorsArgs) (graphqlbackend.MonitorConnectionResolver, error) {
	q, err := monitorsQuery(userID, args)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ms, err := scanMonitors(rows)
	if err != nil {
		return nil, err
	}
	// Hydrate the monitors with the resolver.
	for _, m := range ms {
		m.(*monitor).Resolver = r
	}
	return &monitorConnection{r, ms}, nil
}

func (r *Resolver) CreateCodeMonitor(ctx context.Context, args *graphqlbackend.CreateCodeMonitorArgs) (m graphqlbackend.MonitorResolver, err error) {
	// TODO: check actor is the owner, or site-admin, or part of the owner-org
	// start transaction
	var txStore *basestore.Store
	txStore, err = r.db.Transact(ctx)
	if err != nil {
		return nil, err
	}
	tx := Resolver{
		db:    txStore,
		clock: r.clock,
	}
	defer func() { err = tx.db.Done(err) }()

	// create code monitor
	var q *sqlf.Query
	q, err = tx.createCodeMonitorQuery(ctx, args)
	if err != nil {
		return nil, err
	}
	m, err = tx.runMonitorQuery(ctx, q)
	if err != nil {
		return nil, err
	}

	// create trigger
	q, err = tx.createTriggerQueryQuery(ctx, m.(*monitor).id, args)
	if err != nil {
		return nil, err
	}
	err = tx.db.Exec(ctx, q)
	if err != nil {
		return nil, err
	}

	// create actions
	for i, action := range args.Actions {
		if action.Email != nil {
			q, err = tx.createActionEmailQuery(ctx, m.(*monitor).id, action.Email)
			if err != nil {
				return nil, err
			}
			var e graphqlbackend.MonitorEmailResolver
			e, err = tx.runEmailQuery(ctx, q)
			if err != nil {
				return nil, err
			}

			// insert recipients
			for _, recipient := range action.Email.Recipients {
				q, err = tx.createRecipientQuery(ctx, e.(*monitorEmail).id, recipient)
				if err != nil {
					return nil, err
				}
				err = tx.db.Exec(ctx, q)
				if err != nil {
					return nil, err
				}
			}
		} else {
			return nil, fmt.Errorf("missing email object for action %d", i)
		}
	}
	return m, nil
}

func (r *Resolver) ToggleCodeMonitor(ctx context.Context, args *graphqlbackend.ToggleCodeMonitorArgs) (graphqlbackend.MonitorResolver, error) {
	err := r.isAllowedToEdit(ctx, args.Id)
	if err != nil {
		return nil, fmt.Errorf("ToggleCodeMonitor: %w", err)
	}
	q, err := r.toggleCodeMonitorQuery(ctx, args)
	if err != nil {
		return nil, err
	}
	return r.runMonitorQuery(ctx, q)
}

func (r *Resolver) DeleteCodeMonitor(ctx context.Context, args *graphqlbackend.DeleteCodeMonitorArgs) (*graphqlbackend.EmptyResponse, error) {
	err := r.isAllowedToEdit(ctx, args.Id)
	if err != nil {
		return nil, fmt.Errorf("DeleteCodeMonitor: %w", err)
	}
	q, err := r.deleteCodeMonitorQuery(ctx, args)
	if err != nil {
		return nil, err
	}
	if err := r.db.Exec(ctx, q); err != nil {
		return nil, err
	}
	return &graphqlbackend.EmptyResponse{}, nil
}

func (r *Resolver) runMonitorQuery(ctx context.Context, q *sqlf.Query) (graphqlbackend.MonitorResolver, error) {
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ms, err := scanMonitors(rows)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, fmt.Errorf("operation failed. Query should have returned 1 row")
	}
	return ms[0], nil
}

func (r *Resolver) runEmailQuery(ctx context.Context, q *sqlf.Query) (graphqlbackend.MonitorEmailResolver, error) {
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ms, err := scanEmails(rows)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, fmt.Errorf("operation failed. Query should have returned 1 row")
	}
	return ms[0], nil
}

func (r *Resolver) runTriggerQuery(ctx context.Context, q *sqlf.Query) (graphqlbackend.MonitorQueryResolver, error) {
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ms, err := scanQueries(rows)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, fmt.Errorf("operation failed. Query should have returned 1 row")
	}
	return ms[0], nil
}

func scanMonitors(rows *sql.Rows) ([]graphqlbackend.MonitorResolver, error) {
	var ms []graphqlbackend.MonitorResolver
	for rows.Next() {
		m := &monitor{}
		if err := rows.Scan(
			&m.id,
			&m.createdBy,
			&m.createdAt,
			&m.changedBy,
			&m.changedAt,
			&m.description,
			&m.enabled,
			&m.namespaceUserID,
			&m.namespaceOrgID,
		); err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	err := rows.Close()
	if err != nil {
		return nil, err
	}
	// Rows.Err will report the last error encountered by Rows.Scan.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ms, nil
}

func scanEmails(rows *sql.Rows) ([]graphqlbackend.MonitorEmailResolver, error) {
	var ms []graphqlbackend.MonitorEmailResolver
	for rows.Next() {
		m := &monitorEmail{}
		if err := rows.Scan(
			&m.id,
			&m.monitor,
			&m.enabled,
			&m.priority,
			&m.header,
			&m.createdBy,
			&m.createdAt,
			&m.changedBy,
			&m.changedAt,
		); err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	err := rows.Close()
	if err != nil {
		return nil, err
	}
	// Rows.Err will report the last error encountered by Rows.Scan.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ms, nil
}

func scanQueries(rows *sql.Rows) ([]graphqlbackend.MonitorQueryResolver, error) {
	var ms []graphqlbackend.MonitorQueryResolver
	for rows.Next() {
		m := &monitorQuery{}
		if err := rows.Scan(
			&m.id,
			&m.monitor,
			&m.query,
			&m.createdBy,
			&m.createdAt,
			&m.changedBy,
			&m.changedAt,
		); err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	err := rows.Close()
	if err != nil {
		return nil, err
	}
	// Rows.Err will report the last error encountered by Rows.Scan.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ms, nil
}

func nilOrInt32(n int32) *int32 {
	if n == 0 {
		return nil
	}
	return &n
}

var monitorColumns = []*sqlf.Query{
	sqlf.Sprintf("cm_monitors.id"),
	sqlf.Sprintf("cm_monitors.created_by"),
	sqlf.Sprintf("cm_monitors.created_at"),
	sqlf.Sprintf("cm_monitors.changed_by"),
	sqlf.Sprintf("cm_monitors.changed_at"),
	sqlf.Sprintf("cm_monitors.description"),
	sqlf.Sprintf("cm_monitors.enabled"),
	sqlf.Sprintf("cm_monitors.namespace_user_id"),
	sqlf.Sprintf("cm_monitors.namespace_org_id"),
}

func monitorsQuery(userID int32, args *graphqlbackend.ListMonitorsArgs) (*sqlf.Query, error) {
	const SelectMonitorsByOwner = `
SELECT id, created_by, created_at, changed_by, changed_at, description, enabled, namespace_user_id, namespace_org_id 
FROM cm_monitors
WHERE namespace_user_id = %s
AND id > %s
ORDER BY id ASC
LIMIT %S
`
	var after int64
	if args.After == nil {
		after = 0
	} else {
		err := relay.UnmarshalSpec(graphql.ID(*args.After), &after)
		if err != nil {
			return nil, err
		}
	}
	query := sqlf.Sprintf(
		SelectMonitorsByOwner,
		userID,
		after,
		args.First,
	)
	return query, nil
}

func (r *Resolver) createCodeMonitorQuery(ctx context.Context, args *graphqlbackend.CreateCodeMonitorArgs) (*sqlf.Query, error) {
	const InsertCodeMonitorQuery = `
INSERT INTO cm_monitors 
(created_at, created_by, changed_at, changed_by, description, enabled, namespace_user_id, namespace_org_id) 
VALUES (%s,%s,%s,%s,%s,%s,%s,%s)
RETURNING %s;
`
	var userID int32
	var orgID int32
	err := graphqlbackend.UnmarshalNamespaceID(args.Namespace, &userID, &orgID)
	if err != nil {
		return nil, err
	}
	now := r.clock()
	a := actor.FromContext(ctx)
	return sqlf.Sprintf(
		InsertCodeMonitorQuery,
		now,
		a.UID,
		now,
		a.UID,
		args.Description,
		args.Enabled,
		nilOrInt32(userID),
		nilOrInt32(orgID),
		sqlf.Join(monitorColumns, ", "),
	), nil
}

var queryColumns = []*sqlf.Query{
	sqlf.Sprintf("cm_queries.id"),
	sqlf.Sprintf("cm_queries.monitor"),
	sqlf.Sprintf("cm_queries.query"),
	sqlf.Sprintf("cm_queries.created_by"),
	sqlf.Sprintf("cm_queries.created_at"),
	sqlf.Sprintf("cm_queries.changed_by"),
	sqlf.Sprintf("cm_queries.changed_at"),
}

func (r *Resolver) createTriggerQueryQuery(ctx context.Context, monitorId int64, args *graphqlbackend.CreateCodeMonitorArgs) (*sqlf.Query, error) {
	const insertQueryQuery = `
INSERT INTO cm_queries
(monitor, query, created_by, created_at, changed_by, changed_at)
VALUES (%s,%s,%s,%s,%s,%s)
RETURNING %s;
`
	var userID int32
	var orgID int32
	err := graphqlbackend.UnmarshalNamespaceID(args.Namespace, &userID, &orgID)
	if err != nil {
		return nil, err
	}
	now := r.clock()
	a := actor.FromContext(ctx)
	return sqlf.Sprintf(
		insertQueryQuery,
		monitorId,
		args.Trigger.Query,
		a.UID,
		now,
		a.UID,
		now,
		sqlf.Join(queryColumns, ", "),
	), nil
}

func (r *Resolver) triggerQueryByMonitorQuery(ctx context.Context, monitorID int64) (*sqlf.Query, error) {
	const triggerQueryByMonitorQuery = `
SELECT id, monitor, query, created_by, created_at, changed_by, changed_at
FROM cm_queries
WHERE monitor = %s;
`
	return sqlf.Sprintf(
		triggerQueryByMonitorQuery,
		monitorID,
	), nil
}

var emailsColumns = []*sqlf.Query{
	sqlf.Sprintf("cm_emails.id"),
	sqlf.Sprintf("cm_emails.monitor"),
	sqlf.Sprintf("cm_emails.enabled"),
	sqlf.Sprintf("cm_emails.priority"),
	sqlf.Sprintf("cm_emails.header"),
	sqlf.Sprintf("cm_emails.created_by"),
	sqlf.Sprintf("cm_emails.created_at"),
	sqlf.Sprintf("cm_emails.changed_by"),
	sqlf.Sprintf("cm_emails.changed_at"),
}

func (r *Resolver) createActionEmailQuery(ctx context.Context, monitorId int64, args *graphqlbackend.CreateActionEmailArgs) (*sqlf.Query, error) {
	const insertEmailQuery = `
INSERT INTO cm_emails
(monitor, enabled, priority, header, created_by, created_at, changed_by, changed_at)
VALUES (%s,%s,%s,%s,%s,%s,%s,%s)
RETURNING %s;
`
	now := r.clock()
	a := actor.FromContext(ctx)
	return sqlf.Sprintf(
		insertEmailQuery,
		monitorId,
		args.Enabled,
		args.Priority,
		args.Header,
		a.UID,
		now,
		a.UID,
		now,
		sqlf.Join(emailsColumns, ", "),
	), nil
}

var recipientsColumns = []*sqlf.Query{
	sqlf.Sprintf("cm_recipients.id"),
	sqlf.Sprintf("cm_recipients.email"),
	sqlf.Sprintf("cm_recipients.namespace_user_id"),
	sqlf.Sprintf("cm_recipients.namespace_org_id"),
}

func (r *Resolver) createRecipientQuery(ctx context.Context, emailId int64, namespace graphql.ID) (*sqlf.Query, error) {
	const insertRecipientQuery = `
INSERT INTO cm_recipients
(email, namespace_user_id, namespace_org_id)
VALUES (%s,%s,%s)
RETURNING %s;
`
	var userID int32
	var orgID int32
	err := graphqlbackend.UnmarshalNamespaceID(namespace, &userID, &orgID)
	if err != nil {
		return nil, err
	}
	return sqlf.Sprintf(
		insertRecipientQuery,
		emailId,
		nilOrInt32(userID),
		nilOrInt32(orgID),
		sqlf.Join(recipientsColumns, ", "),
	), nil
}

func (r *Resolver) toggleCodeMonitorQuery(ctx context.Context, args *graphqlbackend.ToggleCodeMonitorArgs) (*sqlf.Query, error) {
	toggleCodeMonitorQuery := `
UPDATE cm_monitors
SET enabled = %s,
	changed_by = %s,
	changed_at = %s
WHERE id = %s
RETURNING %s
`
	var monitorID int64
	err := relay.UnmarshalSpec(args.Id, &monitorID)
	if err != nil {
		return nil, err
	}
	actorUID := actor.FromContext(ctx).UID
	query := sqlf.Sprintf(
		toggleCodeMonitorQuery,
		args.Enabled,
		actorUID,
		r.clock(),
		monitorID,
		sqlf.Join(monitorColumns, ", "),
	)
	return query, nil
}

func (r *Resolver) deleteCodeMonitorQuery(ctx context.Context, args *graphqlbackend.DeleteCodeMonitorArgs) (*sqlf.Query, error) {
	deleteCodeMonitorQuery := `DELETE FROM cm_monitors WHERE id = %s`
	var monitorID int64
	err := relay.UnmarshalSpec(args.Id, &monitorID)
	if err != nil {
		return nil, err
	}
	query := sqlf.Sprintf(
		deleteCodeMonitorQuery,
		monitorID,
	)
	return query, nil
}

// isAllowedToEdit compares the owner of a monitor (user or org) to the actor of
// the request. A user can edit a monitor if either of the following statements
// is true:
// - she is the owner
// - she is a member of the organization which is the owner of the monitor
// - she is a site-admin
func (r *Resolver) isAllowedToEdit(ctx context.Context, id graphql.ID) error {
	var monitorId int32
	err := relay.UnmarshalSpec(id, &monitorId)
	if err != nil {
		return err
	}
	userId, orgID, err := r.ownerForId32(ctx, monitorId)
	if err != nil {
		return err
	}
	if userId == nil && orgID == nil {
		return fmt.Errorf("monitor does not exist")
	}
	if orgID != nil {
		if err := backend.CheckOrgAccess(ctx, *orgID); err != nil {
			return fmt.Errorf("user is not a member of the organization which owns the code monitor")
		}
		return nil
	}
	if err := backend.CheckSiteAdminOrSameUser(ctx, *userId); err != nil {
		return fmt.Errorf("user not allowed to edit")
	}
	return nil
}

func (r *Resolver) ownerForId32(ctx context.Context, monitorId int32) (userId *int32, orgId *int32, err error) {
	var (
		q    *sqlf.Query
		rows *sql.Rows
	)
	q, err = ownerForId32Query(ctx, monitorId)
	if err != nil {
		return nil, nil, err
	}
	rows, err = r.db.Query(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		if err = rows.Scan(
			&userId,
			&orgId,
		); err != nil {
			return nil, nil, err
		}
	}
	err = rows.Close()
	if err != nil {
		return nil, nil, err
	}

	// Rows.Err will report the last error encountered by Rows.Scan.
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	return userId, orgId, nil
}

func ownerForId32Query(ctx context.Context, monitorId int32) (*sqlf.Query, error) {
	const ownerForId32Query = `SELECT namespace_user_id, namespace_org_id FROM cm_monitors WHERE id = %s`
	return sqlf.Sprintf(
		ownerForId32Query,
		monitorId,
	), nil
}

//
// MonitorConnection
//
type monitorConnection struct {
	*Resolver
	monitors []graphqlbackend.MonitorResolver
}

func (m *monitorConnection) Nodes(ctx context.Context) ([]graphqlbackend.MonitorResolver, error) {
	return m.monitors, nil
}

func (m *monitorConnection) TotalCount(ctx context.Context) (int32, error) {
	return int32(len(m.monitors)), nil
}

func (m *monitorConnection) PageInfo(ctx context.Context) (*graphqlutil.PageInfo, error) {
	if len(m.monitors) == 0 {
		return graphqlutil.HasNextPage(false), nil
	}
	return graphqlutil.NextPageCursor(string(m.monitors[len(m.monitors)-1].ID())), nil
}

//
// Monitor
//
type monitor struct {
	*Resolver
	id              int64
	createdBy       int32
	createdAt       time.Time
	changedBy       int32
	changedAt       time.Time
	description     string
	enabled         bool
	namespaceUserID *int32
	namespaceOrgID  *int32
}

const (
	monitorKind = "CodeMonitor"
	triggerKind = "CodeMonitorTrigger"
)

func (m *monitor) ID() graphql.ID {
	return relay.MarshalID(monitorKind, m.id)
}

func (m *monitor) CreatedBy(ctx context.Context) (*graphqlbackend.UserResolver, error) {
	return graphqlbackend.UserByIDInt32(ctx, m.createdBy)
}

func (m *monitor) CreatedAt() graphqlbackend.DateTime {
	return graphqlbackend.DateTime{Time: m.createdAt}
}

func (m *monitor) Description() string {
	return m.description
}

func (m *monitor) Enabled() bool {
	return m.enabled
}

func (m *monitor) Owner(ctx context.Context) (n graphqlbackend.NamespaceResolver, err error) {
	if m.namespaceOrgID == nil {
		n.Namespace, err = graphqlbackend.UserByIDInt32(ctx, *m.namespaceUserID)
	} else {
		n.Namespace, err = graphqlbackend.OrgByIDInt32(ctx, *m.namespaceOrgID)
	}
	return n, err
}

func (m *monitor) Trigger(ctx context.Context) (graphqlbackend.MonitorTrigger, error) {
	q, err := m.triggerQueryByMonitorQuery(ctx, m.id)
	if err != nil {
		return nil, err
	}
	t, err := m.runTriggerQuery(ctx, q)
	if err != nil {
		return nil, err
	}
	// Hydrate with resolver.
	t.(*monitorQuery).Resolver = m.Resolver
	return &monitorTrigger{t}, nil
}

func (m *monitor) Actions(ctx context.Context, args *graphqlbackend.ListActionArgs) (graphqlbackend.MonitorActionConnectionResolver, error) {
	return &monitorActionConnection{
			monitorID: m.ID(),
			userID:    m.ID()},
		nil
}

//
// MonitorTrigger <<UNION>>
//
type monitorTrigger struct {
	query graphqlbackend.MonitorQueryResolver
}

func (t *monitorTrigger) ToMonitorQuery() (graphqlbackend.MonitorQueryResolver, bool) {
	return t.query, t.query != nil
}

//
// Query
//
type monitorQuery struct {
	*Resolver
	id        int64
	monitor   int64
	query     string
	createdBy int64
	createdAt time.Time
	changedBy int64
	changedAt time.Time
}

func (q *monitorQuery) ID() graphql.ID {
	return relay.MarshalID(triggerKind, q.id)
}

func (q *monitorQuery) Query() string {
	return q.query
}

func (q *monitorQuery) Events(ctx context.Context, args *graphqlbackend.ListEventsArgs) graphqlbackend.MonitorTriggerEventConnectionResolver {
	return &monitorTriggerEventConnection{monitorID: relay.MarshalID(monitorKind, q.monitor), userID: relay.MarshalID("User", actor.FromContext(ctx).UID)}
}

//
// MonitorTriggerEventConnection
//
type monitorTriggerEventConnection struct {
	monitorID graphql.ID
	userID    graphql.ID // TODO: remove this. Just for stub implementation
}

func (a *monitorTriggerEventConnection) Nodes(ctx context.Context) ([]graphqlbackend.MonitorTriggerEventResolver, error) {
	return []graphqlbackend.MonitorTriggerEventResolver{&monitorTriggerEvent{
		id:        "42",
		status:    "SUCCESS",
		message:   nil,
		timestamp: graphqlbackend.DateTime{Time: time.Now()},
		monitorID: a.monitorID,
		userID:    a.userID,
	}}, nil
}

func (a *monitorTriggerEventConnection) TotalCount(ctx context.Context) (int32, error) {
	return 1, nil
}

func (a *monitorTriggerEventConnection) PageInfo(ctx context.Context) (*graphqlutil.PageInfo, error) {
	return graphqlutil.HasNextPage(false), nil
}

//
// MonitorTriggerEvent
//
type monitorTriggerEvent struct {
	id        graphql.ID
	status    string
	message   *string
	timestamp graphqlbackend.DateTime
	monitorID graphql.ID

	userID graphql.ID // TODO: remove this. Just for stub implementation
}

func (m *monitorTriggerEvent) ID() graphql.ID {
	return m.id
}

func (m *monitorTriggerEvent) Status() string {
	return m.status
}

func (m *monitorTriggerEvent) Message() *string {
	return m.message
}

func (m *monitorTriggerEvent) Timestamp() graphqlbackend.DateTime {
	return m.timestamp
}

func (m *monitorTriggerEvent) Actions(ctx context.Context, args *graphqlbackend.ListActionArgs) (graphqlbackend.MonitorActionConnectionResolver, error) {
	return graphqlbackend.MonitorActionConnectionResolver(&monitorActionConnection{userID: m.userID, monitorID: m.monitorID, triggerEventID: &m.id}), nil
}

// ActionConnection
//
type monitorActionConnection struct {
	userID    graphql.ID //  TODO: remove this. This is just for the stub implementation.
	monitorID graphql.ID

	// triggerEventID is used to link action events to a trigger event
	triggerEventID *graphql.ID
}

func (a *monitorActionConnection) Nodes(ctx context.Context) ([]graphqlbackend.MonitorAction, error) {
	return []graphqlbackend.MonitorAction{&action{email: &monitorEmail{id: 42}}}, nil
}

func (a *monitorActionConnection) TotalCount(ctx context.Context) (int32, error) {
	return 1, nil
}

func (a *monitorActionConnection) PageInfo(ctx context.Context) (*graphqlutil.PageInfo, error) {
	return graphqlutil.HasNextPage(false), nil
}

//
// Action <<UNION>>
//
type action struct {
	email graphqlbackend.MonitorEmailResolver
}

func (a *action) ToMonitorEmail() (graphqlbackend.MonitorEmailResolver, bool) {
	return a.email, a.email != nil
}

//
// Email
//
type monitorEmail struct {
	id        int64
	monitor   int64
	enabled   bool
	priority  string
	header    string
	createdBy int32
	createdAt time.Time
	changedBy int32
	changedAt time.Time

	// If triggerEventID == nil, all events of this action will be returned.
	// Otherwise, only those events of this action which are related to the specified
	// trigger event will be returned.
	triggerEventID *graphql.ID
}

func (m *monitorEmail) Recipients(ctx context.Context, args *graphqlbackend.ListRecipientsArgs) (c graphqlbackend.MonitorActionEmailRecipientsConnectionResolver, err error) {
	n := graphqlbackend.NamespaceResolver{}
	// dummy data
	n.Namespace, err = graphqlbackend.UserByIDInt32(ctx, actor.FromContext(ctx).UID)
	if err != nil {
		return nil, err
	}
	return &monitorActionEmailRecipientConnection{
		recipients: []graphqlbackend.NamespaceResolver{n},
	}, nil
}

func (m *monitorEmail) Enabled() bool {
	return true
}

func (m *monitorEmail) Priority() string {
	return "NORMAL"
}

func (m *monitorEmail) Header() string {
	return "Header not implemented"
}

func (m *monitorEmail) ID() graphql.ID {
	return "monitorEmail ID not implemented"
}

func (m *monitorEmail) Events(ctx context.Context, args *graphqlbackend.ListEventsArgs) (graphqlbackend.MonitorActionEventConnectionResolver, error) {
	return &monitorActionEventConnection{}, nil
}

//
// MonitorActionEmailRecipientConnection
//
type monitorActionEmailRecipientConnection struct {
	recipients []graphqlbackend.NamespaceResolver
}

func (a *monitorActionEmailRecipientConnection) Nodes(ctx context.Context) ([]graphqlbackend.NamespaceResolver, error) {
	return a.recipients, nil
}

func (a *monitorActionEmailRecipientConnection) TotalCount(ctx context.Context) (int32, error) {
	return 1, nil
}

func (a *monitorActionEmailRecipientConnection) PageInfo(ctx context.Context) (*graphqlutil.PageInfo, error) {
	return graphqlutil.HasNextPage(false), nil
}

//
// MonitorActionEventConnection
//
type monitorActionEventConnection struct {
}

func (a *monitorActionEventConnection) Nodes(ctx context.Context) ([]graphqlbackend.MonitorActionEventResolver, error) {
	notImplemented := "message not implemented"
	return []graphqlbackend.MonitorActionEventResolver{
			&monitorActionEvent{id: "314", status: "SUCCESS", timestamp: graphqlbackend.DateTime{Time: time.Now()}},
			&monitorActionEvent{id: "315", status: "ERROR", message: &notImplemented, timestamp: graphqlbackend.DateTime{Time: time.Now()}},
		},
		nil
}

func (a *monitorActionEventConnection) TotalCount(ctx context.Context) (int32, error) {
	return 1, nil
}

func (a *monitorActionEventConnection) PageInfo(ctx context.Context) (*graphqlutil.PageInfo, error) {
	return graphqlutil.HasNextPage(false), nil
}

//
// MonitorEvent
//
type monitorActionEvent struct {
	id        graphql.ID
	status    string
	message   *string
	timestamp graphqlbackend.DateTime
}

func (m *monitorActionEvent) ID() graphql.ID {
	return m.id
}

func (m *monitorActionEvent) Status() string {
	return m.status
}

func (m *monitorActionEvent) Message() *string {
	return m.message
}

func (m *monitorActionEvent) Timestamp() graphqlbackend.DateTime {
	return m.timestamp
}
