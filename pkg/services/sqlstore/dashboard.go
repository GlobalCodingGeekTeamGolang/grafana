package sqlstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/models"
	ac "github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/search"
	"github.com/grafana/grafana/pkg/services/sqlstore/permissions"
	"github.com/grafana/grafana/pkg/services/sqlstore/searchstore"
	"github.com/grafana/grafana/pkg/util"
)

var shadowSearchCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Subsystem: "db_dashboard",
		Name:      "search_shadow",
	},
	[]string{"equal", "error"},
)

func init() {
	prometheus.MustRegister(shadowSearchCounter)
}

func (ss *SQLStore) addDashboardQueryAndCommandHandlers() {
	bus.AddHandler("sql", ss.GetDashboard)
	bus.AddHandler("sql", ss.GetDashboardUIDById)
	bus.AddHandler("sql", ss.GetDashboardTags)
	bus.AddHandler("sql", ss.SearchDashboards)
	bus.AddHandler("sql", ss.DeleteDashboard)
	bus.AddHandler("sql", ss.GetDashboards)
	bus.AddHandler("sql", ss.HasEditPermissionInFolders)
	bus.AddHandler("sql", ss.GetDashboardPermissionsForUser)
	bus.AddHandler("sql", ss.GetDashboardsByPluginId)
	bus.AddHandler("sql", ss.GetDashboardSlugById)
	bus.AddHandler("sql", ss.HasAdminPermissionInFolders)
}

var generateNewUid func() string = util.GenerateShortUID

func (ss *SQLStore) GetDashboard(ctx context.Context, query *models.GetDashboardQuery) error {
	return ss.WithDbSession(ctx, func(dbSession *DBSession) error {
		if query.Id == 0 && len(query.Slug) == 0 && len(query.Uid) == 0 {
			return models.ErrDashboardIdentifierNotSet
		}

		dashboard := models.Dashboard{Slug: query.Slug, OrgId: query.OrgId, Id: query.Id, Uid: query.Uid}
		has, err := dbSession.Get(&dashboard)

		if err != nil {
			return err
		} else if !has {
			return models.ErrDashboardNotFound
		}

		dashboard.SetId(dashboard.Id)
		dashboard.SetUid(dashboard.Uid)
		query.Result = &dashboard
		return nil
	})
}

type DashboardSearchProjection struct {
	ID          int64  `xorm:"id"`
	UID         string `xorm:"uid"`
	Title       string
	Slug        string
	Term        string
	IsFolder    bool
	FolderID    int64  `xorm:"folder_id"`
	FolderUID   string `xorm:"folder_uid"`
	FolderSlug  string
	FolderTitle string
	SortMeta    int64
}

func (ss *SQLStore) FindDashboards(ctx context.Context, query *search.FindPersistedDashboardsQuery) ([]DashboardSearchProjection, error) {
	filters := []interface{}{
		permissions.DashboardPermissionFilter{
			OrgRole:         query.SignedInUser.OrgRole,
			OrgId:           query.SignedInUser.OrgId,
			Dialect:         dialect,
			UserId:          query.SignedInUser.UserId,
			PermissionLevel: query.Permission,
		},
	}

	if ss.Cfg.IsFeatureToggleEnabled("accesscontrol") {
		filters = []interface{}{
			permissions.AccessControlDashboardPermissionFilter{User: query.SignedInUser, PermissionLevel: query.Permission},
		}
	}

	for _, filter := range query.Sort.Filter {
		filters = append(filters, filter)
	}

	if query.OrgId != 0 {
		filters = append(filters, searchstore.OrgFilter{OrgId: query.OrgId})
	} else if query.SignedInUser.OrgId != 0 {
		filters = append(filters, searchstore.OrgFilter{OrgId: query.SignedInUser.OrgId})
	}

	if len(query.Tags) > 0 {
		filters = append(filters, searchstore.TagsFilter{Tags: query.Tags})
	}

	if len(query.DashboardIds) > 0 {
		filters = append(filters, searchstore.DashboardFilter{IDs: query.DashboardIds})
	}

	if query.IsStarred {
		filters = append(filters, searchstore.StarredFilter{UserId: query.SignedInUser.UserId})
	}

	if len(query.Title) > 0 {
		filters = append(filters, searchstore.TitleFilter{Dialect: dialect, Title: query.Title})
	}

	if len(query.Type) > 0 {
		filters = append(filters, searchstore.TypeFilter{Dialect: dialect, Type: query.Type})
	}

	if len(query.FolderIds) > 0 {
		filters = append(filters, searchstore.FolderFilter{IDs: query.FolderIds})
	}

	var res []DashboardSearchProjection
	sb := &searchstore.Builder{Dialect: dialect, Filters: filters}

	limit := query.Limit
	if limit < 1 {
		limit = 1000
	}

	page := query.Page
	if page < 1 {
		page = 1
	}

	sql, params := sb.ToSQL(limit, page)

	err := ss.WithDbSession(ctx, func(dbSession *DBSession) error {
		return dbSession.SQL(sql, params...).Find(&res)
	})

	if err != nil {
		return nil, err
	}

	return res, nil
}

func (ss *SQLStore) SearchDashboards(ctx context.Context, query *search.FindPersistedDashboardsQuery) error {
	res, err := ss.FindDashboards(ctx, query)
	if err != nil {
		return err
	}

	makeQueryResult(query, res)

	return nil
}

func getHitType(item DashboardSearchProjection) search.HitType {
	var hitType search.HitType
	if item.IsFolder {
		hitType = search.DashHitFolder
	} else {
		hitType = search.DashHitDB
	}

	return hitType
}

func makeQueryResult(query *search.FindPersistedDashboardsQuery, res []DashboardSearchProjection) {
	query.Result = make([]*search.Hit, 0)
	hits := make(map[int64]*search.Hit)

	for _, item := range res {
		hit, exists := hits[item.ID]
		if !exists {
			hit = &search.Hit{
				ID:          item.ID,
				UID:         item.UID,
				Title:       item.Title,
				URI:         "db/" + item.Slug,
				URL:         models.GetDashboardFolderUrl(item.IsFolder, item.UID, item.Slug),
				Type:        getHitType(item),
				FolderID:    item.FolderID,
				FolderUID:   item.FolderUID,
				FolderTitle: item.FolderTitle,
				Tags:        []string{},
			}

			if item.FolderID > 0 {
				hit.FolderURL = models.GetFolderUrl(item.FolderUID, item.FolderSlug)
			}

			if query.Sort.MetaName != "" {
				hit.SortMeta = item.SortMeta
				hit.SortMetaName = query.Sort.MetaName
			}

			query.Result = append(query.Result, hit)
			hits[item.ID] = hit
		}
		if len(item.Term) > 0 {
			hit.Tags = append(hit.Tags, item.Term)
		}
	}
}

func (ss *SQLStore) GetDashboardTags(ctx context.Context, query *models.GetDashboardTagsQuery) error {
	return ss.WithDbSession(ctx, func(dbSession *DBSession) error {
		sql := `SELECT
					  COUNT(*) as count,
						term
					FROM dashboard
					INNER JOIN dashboard_tag on dashboard_tag.dashboard_id = dashboard.id
					WHERE dashboard.org_id=?
					GROUP BY term
					ORDER BY term`

		query.Result = make([]*models.DashboardTagCloudItem, 0)
		sess := dbSession.SQL(sql, query.OrgId)
		err := sess.Find(&query.Result)
		return err
	})
}

func (ss *SQLStore) DeleteDashboard(ctx context.Context, cmd *models.DeleteDashboardCommand) error {
	return ss.WithTransactionalDbSession(ctx, func(sess *DBSession) error {
		return deleteDashboard(cmd, sess)
	})
}

func deleteDashboard(cmd *models.DeleteDashboardCommand, sess *DBSession) error {
	dashboard := models.Dashboard{Id: cmd.Id, OrgId: cmd.OrgId}
	has, err := sess.Get(&dashboard)
	if err != nil {
		return err
	} else if !has {
		return models.ErrDashboardNotFound
	}

	deletes := []string{
		"DELETE FROM dashboard_tag WHERE dashboard_id = ? ",
		"DELETE FROM star WHERE dashboard_id = ? ",
		"DELETE FROM dashboard WHERE id = ?",
		"DELETE FROM playlist_item WHERE type = 'dashboard_by_id' AND value = ?",
		"DELETE FROM dashboard_version WHERE dashboard_id = ?",
		"DELETE FROM annotation WHERE dashboard_id = ?",
		"DELETE FROM dashboard_provisioning WHERE dashboard_id = ?",
		"DELETE FROM dashboard_acl WHERE dashboard_id = ?",
	}

	if dashboard.IsFolder {
		deletes = append(deletes, "DELETE FROM dashboard WHERE folder_id = ?")

		dashIds := []struct {
			Id int64
		}{}
		err := sess.SQL("SELECT id FROM dashboard WHERE folder_id = ?", dashboard.Id).Find(&dashIds)
		if err != nil {
			return err
		}

		for _, id := range dashIds {
			if err := deleteAlertDefinition(id.Id, sess); err != nil {
				return err
			}
		}

		// remove all access control permission with folder scope
		_, err = sess.Exec("DELETE FROM permission WHERE scope = ?", ac.Scope("folders", "id", strconv.FormatInt(dashboard.Id, 10)))
		if err != nil {
			return err
		}

		for _, dash := range dashIds {
			// remove all access control permission with child dashboard scopes
			_, err = sess.Exec("DELETE FROM permission WHERE scope = ?", ac.Scope("dashboards", "id", strconv.FormatInt(dash.Id, 10)))
			if err != nil {
				return err
			}
		}

		if len(dashIds) > 0 {
			childrenDeletes := []string{
				"DELETE FROM dashboard_tag WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM star WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM dashboard_version WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM annotation WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM dashboard_provisioning WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM dashboard_acl WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
			}
			for _, sql := range childrenDeletes {
				_, err := sess.Exec(sql, dashboard.OrgId, dashboard.Id)
				if err != nil {
					return err
				}
			}
		}

		var existingRuleID int64
		exists, err := sess.Table("alert_rule").Where("namespace_uid = (SELECT uid FROM dashboard WHERE id = ?)", dashboard.Id).Cols("id").Get(&existingRuleID)
		if err != nil {
			return err
		}
		if exists {
			if !cmd.ForceDeleteFolderRules {
				return fmt.Errorf("folder cannot be deleted: %w", models.ErrFolderContainsAlertRules)
			}

			// Delete all rules under this folder.
			deleteNGAlertsByFolder := []string{
				"DELETE FROM alert_rule WHERE namespace_uid = (SELECT uid FROM dashboard WHERE id = ?)",
				"DELETE FROM alert_rule_version WHERE rule_namespace_uid = (SELECT uid FROM dashboard WHERE id = ?)",
			}

			for _, sql := range deleteNGAlertsByFolder {
				_, err := sess.Exec(sql, dashboard.Id)
				if err != nil {
					return err
				}
			}
		}
	} else {
		_, err = sess.Exec("DELETE FROM permission WHERE scope = ?", ac.Scope("dashboards", "id", strconv.FormatInt(dashboard.Id, 10)))
		if err != nil {
			return err
		}
	}

	if err := deleteAlertDefinition(dashboard.Id, sess); err != nil {
		return err
	}

	for _, sql := range deletes {
		_, err := sess.Exec(sql, dashboard.Id)
		if err != nil {
			return err
		}
	}

	return nil
}

func (ss *SQLStore) GetDashboards(ctx context.Context, query *models.GetDashboardsQuery) error {
	return ss.WithDbSession(ctx, func(dbSession *DBSession) error {
		if len(query.DashboardIds) == 0 {
			return models.ErrCommandValidationFailed
		}

		var dashboards = make([]*models.Dashboard, 0)

		err := dbSession.In("id", query.DashboardIds).Find(&dashboards)
		query.Result = dashboards
		return err
	})
}

// GetDashboardPermissionsForUser returns the maximum permission the specified user has for a dashboard(s)
// The function takes in a list of dashboard ids and the user id and role
func (ss *SQLStore) GetDashboardPermissionsForUser(ctx context.Context, query *models.GetDashboardPermissionsForUserQuery) error {
	return ss.WithDbSession(ctx, func(dbSession *DBSession) error {
		if len(query.DashboardIds) == 0 {
			return models.ErrCommandValidationFailed
		}

		if query.OrgRole == models.ROLE_ADMIN {
			var permissions = make([]*models.DashboardPermissionForUser, 0)
			for _, d := range query.DashboardIds {
				permissions = append(permissions, &models.DashboardPermissionForUser{
					DashboardId:    d,
					Permission:     models.PERMISSION_ADMIN,
					PermissionName: models.PERMISSION_ADMIN.String(),
				})
			}
			query.Result = permissions

			return nil
		}

		params := make([]interface{}, 0)

		// check dashboards that have ACLs via user id, team id or role
		sql := `SELECT d.id AS dashboard_id, MAX(COALESCE(da.permission, pt.permission)) AS permission
	FROM dashboard AS d
		LEFT JOIN dashboard_acl as da on d.folder_id = da.dashboard_id or d.id = da.dashboard_id
		LEFT JOIN team_member as ugm on ugm.team_id =  da.team_id
		LEFT JOIN org_user ou ON ou.role = da.role AND ou.user_id = ?
	`
		params = append(params, query.UserId)

		// check the user's role for dashboards that do not have hasAcl set
		sql += `LEFT JOIN org_user ouRole ON ouRole.user_id = ? AND ouRole.org_id = ?`
		params = append(params, query.UserId)
		params = append(params, query.OrgId)

		sql += `
		LEFT JOIN (SELECT 1 AS permission, 'Viewer' AS role
			UNION SELECT 2 AS permission, 'Editor' AS role
			UNION SELECT 4 AS permission, 'Admin' AS role) pt ON ouRole.role = pt.role
	WHERE
	d.Id IN (?` + strings.Repeat(",?", len(query.DashboardIds)-1) + `) `
		for _, id := range query.DashboardIds {
			params = append(params, id)
		}

		sql += ` AND
	d.org_id = ? AND
	  (
		(d.has_acl = ?  AND (da.user_id = ? OR ugm.user_id = ? OR ou.id IS NOT NULL))
		OR (d.has_acl = ? AND ouRole.id IS NOT NULL)
	)
	group by d.id
	order by d.id asc`
		params = append(params, query.OrgId)
		params = append(params, dialect.BooleanStr(true))
		params = append(params, query.UserId)
		params = append(params, query.UserId)
		params = append(params, dialect.BooleanStr(false))

		err := dbSession.SQL(sql, params...).Find(&query.Result)

		for _, p := range query.Result {
			p.PermissionName = p.Permission.String()
		}

		return err
	})
}

func (ss *SQLStore) GetDashboardsByPluginId(ctx context.Context, query *models.GetDashboardsByPluginIdQuery) error {
	return ss.WithDbSession(ctx, func(dbSession *DBSession) error {
		var dashboards = make([]*models.Dashboard, 0)
		whereExpr := "org_id=? AND plugin_id=? AND is_folder=" + dialect.BooleanStr(false)

		err := dbSession.Where(whereExpr, query.OrgId, query.PluginId).Find(&dashboards)
		query.Result = dashboards
		return err
	})
}

type DashboardSlugDTO struct {
	Slug string
}

func (ss *SQLStore) GetDashboardSlugById(ctx context.Context, query *models.GetDashboardSlugByIdQuery) error {
	return ss.WithDbSession(ctx, func(dbSession *DBSession) error {
		var rawSQL = `SELECT slug from dashboard WHERE Id=?`
		var slug = DashboardSlugDTO{}

		exists, err := dbSession.SQL(rawSQL, query.Id).Get(&slug)

		if err != nil {
			return err
		} else if !exists {
			return models.ErrDashboardNotFound
		}

		query.Result = slug.Slug
		return nil
	})
}

func (ss *SQLStore) GetDashboardUIDById(ctx context.Context, query *models.GetDashboardRefByIdQuery) error {
	return ss.WithDbSession(ctx, func(dbSession *DBSession) error {
		var rawSQL = `SELECT uid, slug from dashboard WHERE Id=?`

		us := &models.DashboardRef{}

		exists, err := dbSession.SQL(rawSQL, query.Id).Get(us)

		if err != nil {
			return err
		} else if !exists {
			return models.ErrDashboardNotFound
		}

		query.Result = us
		return nil
	})
}

// HasEditPermissionInFolders validates that an user have access to a certain folder
func (ss *SQLStore) HasEditPermissionInFolders(ctx context.Context, query *models.HasEditPermissionInFoldersQuery) error {
	return ss.WithDbSession(ctx, func(dbSession *DBSession) error {
		if query.SignedInUser.HasRole(models.ROLE_EDITOR) {
			query.Result = true
			return nil
		}

		builder := &SQLBuilder{}
		builder.Write("SELECT COUNT(dashboard.id) AS count FROM dashboard WHERE dashboard.org_id = ? AND dashboard.is_folder = ?",
			query.SignedInUser.OrgId, dialect.BooleanStr(true))
		builder.WriteDashboardPermissionFilter(query.SignedInUser, models.PERMISSION_EDIT)

		type folderCount struct {
			Count int64
		}

		resp := make([]*folderCount, 0)
		if err := dbSession.SQL(builder.GetSQLString(), builder.params...).Find(&resp); err != nil {
			return err
		}

		query.Result = len(resp) > 0 && resp[0].Count > 0

		return nil
	})
}

func (ss *SQLStore) HasAdminPermissionInFolders(ctx context.Context, query *models.HasAdminPermissionInFoldersQuery) error {
	return ss.WithDbSession(ctx, func(dbSession *DBSession) error {
		if query.SignedInUser.HasRole(models.ROLE_ADMIN) {
			query.Result = true
			return nil
		}

		builder := &SQLBuilder{}
		builder.Write("SELECT COUNT(dashboard.id) AS count FROM dashboard WHERE dashboard.org_id = ? AND dashboard.is_folder = ?", query.SignedInUser.OrgId, dialect.BooleanStr(true))
		builder.WriteDashboardPermissionFilter(query.SignedInUser, models.PERMISSION_ADMIN)

		type folderCount struct {
			Count int64
		}

		resp := make([]*folderCount, 0)
		if err := dbSession.SQL(builder.GetSQLString(), builder.params...).Find(&resp); err != nil {
			return err
		}

		query.Result = len(resp) > 0 && resp[0].Count > 0

		return nil
	})
}
