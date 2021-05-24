package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/listmonk/internal/subimporter"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo"
	"github.com/lib/pq"
)

type bouncesWrap struct {
	Results []models.Bounce `json:"results"`

	Total   int `json:"total"`
	PerPage int `json:"per_page"`
	Page    int `json:"page"`
}

// handleGetBounces handles retrieval of bounce records.
func handleGetBounces(c echo.Context) error {
	var (
		app = c.Get("app").(*App)
		pg  = getPagination(c.QueryParams(), 20, 50)
		out bouncesWrap

		id, _     = strconv.Atoi(c.Param("id"))
		campID, _ = strconv.Atoi(c.QueryParam("campaign_id"))
		source    = c.FormValue("source")
		orderBy   = c.FormValue("order_by")
		order     = c.FormValue("order")
	)

	// Fetch one list.
	single := false
	if id > 0 {
		single = true
	}

	// Sort params.
	if !strSliceContains(orderBy, bounceQuerySortFields) {
		orderBy = "created_at"
	}
	if order != sortAsc && order != sortDesc {
		order = sortDesc
	}

	stmt := fmt.Sprintf(app.queries.QueryBounces, orderBy, order)
	if err := db.Select(&out.Results, stmt, id, campID, 0, source, pg.Offset, pg.Limit); err != nil {
		app.log.Printf("error fetching bounces: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			app.i18n.Ts("globals.messages.errorFetching",
				"name", "{globals.terms.bounce}", "error", pqErrMsg(err)))
	}
	if len(out.Results) == 0 {
		out.Results = []models.Bounce{}
		return c.JSON(http.StatusOK, okResp{out})
	}

	if single {
		return c.JSON(http.StatusOK, okResp{out.Results[0]})
	}

	// Meta.
	out.Total = out.Results[0].Total
	out.Page = pg.Page
	out.PerPage = pg.PerPage

	return c.JSON(http.StatusOK, okResp{out})
}

// handleGetSubscriberBounces retrieves a subscriber's bounce records.
func handleGetSubscriberBounces(c echo.Context) error {
	var (
		app   = c.Get("app").(*App)
		subID = c.Param("id")
	)

	id, _ := strconv.ParseInt(subID, 10, 64)
	if id < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidID"))
	}

	out := []models.Bounce{}
	stmt := fmt.Sprintf(app.queries.QueryBounces, "created_at", "ASC")
	if err := db.Select(&out, stmt, 0, 0, subID, "", 0, 1000); err != nil {
		app.log.Printf("error fetching bounces: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			app.i18n.Ts("globals.messages.errorFetching",
				"name", "{globals.terms.bounce}", "error", pqErrMsg(err)))
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// handleDeleteBounces handles bounce deletion, either a single one (ID in the URI), or a list.
func handleDeleteBounces(c echo.Context) error {
	var (
		app    = c.Get("app").(*App)
		pID    = c.Param("id")
		all, _ = strconv.ParseBool(c.QueryParam("all"))
		IDs    = pq.Int64Array{}
	)

	// Is it an /:id call?
	if pID != "" {
		id, _ := strconv.ParseInt(pID, 10, 64)
		if id < 1 {
			return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidID"))
		}
		IDs = append(IDs, id)
	} else if !all {
		// Multiple IDs.
		i, err := parseStringIDs(c.Request().URL.Query()["id"])
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest,
				app.i18n.Ts("globals.messages.invalidID", "error", err.Error()))
		}

		if len(i) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest,
				app.i18n.Ts("globals.messages.invalidID"))
		}
		IDs = i
	}

	if _, err := app.queries.DeleteBounces.Exec(IDs); err != nil {
		app.log.Printf("error deleting bounces: %v", err)
		return echo.NewHTTPError(http.StatusInternalServerError,
			app.i18n.Ts("globals.messages.errorDeleting",
				"name", "{globals.terms.bounce}", "error", pqErrMsg(err)))
	}

	return c.JSON(http.StatusOK, okResp{true})
}

// handleBounceWebhook renders the HTML preview of a template.
func handleBounceWebhook(c echo.Context) error {
	var (
		app     = c.Get("app").(*App)
		service = c.Param("service")

		bounces []models.Bounce
	)

	// Read the request body instead of using using c.Bind() to read to save the entire raw request as meta.
	rawReq, err := ioutil.ReadAll(c.Request().Body)
	if err != nil {
		app.log.Printf("error reading ses notification body: %v", err)
		return echo.NewHTTPError(http.StatusBadRequest, app.i18n.Ts("globals.messages.internalError"))
	}

	switch true {
	// Native internal webhook.
	case service == "":
		var b models.Bounce
		if err := json.Unmarshal(rawReq, &b); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, app.i18n.Ts("globals.messages.invalidData"))
		}

		if err := validateBounceFields(b, app); err != nil {
			return err
		}

		b.Email = strings.ToLower(b.Email)

		if len(b.Meta) == 0 {
			b.Meta = json.RawMessage("{}")
		}

		if b.CreatedAt.Year() == 0 {
			b.CreatedAt = time.Now()
		}

		bounces = append(bounces, b)

	// Amazon SES.
	case service == "ses" && app.constants.BounceSESEnabled:
		switch c.Request().Header.Get("X-Amz-Sns-Message-Type") {
		// SNS webhook registration confirmation. Only after these are processed will the endpoint
		// start getting bounce notifications.
		case "SubscriptionConfirmation", "UnsubscribeConfirmation":
			if err := app.bounce.SES.ProcessSubscription(rawReq); err != nil {
				app.log.Printf("error processing SNS (SES) subscription: %v", err)
				return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidData"))
			}
			break

		// Bounce notification.
		case "Notification":
			b, err := app.bounce.SES.ProcessBounce(rawReq)
			if err != nil {
				app.log.Printf("error processing SES notification: %v", err)
				return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidData"))
			}
			bounces = append(bounces, b)

		default:
			return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidData"))
		}

	// SendGrid.
	case service == "sendgrid" && app.constants.BounceSendgridEnabled:
		var (
			sig = c.Request().Header.Get("X-Twilio-Email-Event-Webhook-Signature")
			ts  = c.Request().Header.Get("X-Twilio-Email-Event-Webhook-Timestamp")
		)

		// Sendgrid sends multiple bounces.
		bs, err := app.bounce.Sendgrid.ProcessBounce(sig, ts, rawReq)
		if err != nil {
			app.log.Printf("error processing sendgrid notification: %v", err)
			return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidData"))
		}
		bounces = append(bounces, bs...)

	default:
		return echo.NewHTTPError(http.StatusBadRequest, app.i18n.Ts("bounces.unknownService"))
	}

	// Record bounces if any.
	for _, b := range bounces {
		if err := app.bounce.Record(b); err != nil {
			app.log.Printf("error recording bounce: %v", err)
		}
	}

	return c.JSON(http.StatusOK, okResp{true})
}

func validateBounceFields(b models.Bounce, app *App) error {
	if b.Email == "" && b.SubscriberUUID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidData"))
	}

	if b.Email != "" && !subimporter.IsEmail(b.Email) {
		return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidEmail"))
	}

	if b.SubscriberUUID != "" && !reUUID.MatchString(b.SubscriberUUID) {
		return echo.NewHTTPError(http.StatusBadRequest, app.i18n.T("globals.messages.invalidUUID"))
	}

	return nil
}
