package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/warm3snow/llm-gateway/internal/billing"
	"github.com/warm3snow/llm-gateway/internal/middleware"
	"github.com/warm3snow/llm-gateway/internal/models"
	"github.com/warm3snow/llm-gateway/internal/types"
	"gorm.io/gorm"
)

type BillingHandler struct {
	Store *billing.Store
	DB    *gorm.DB
}

func (h *BillingHandler) RegisterRoutesWithAuth(r *gin.Engine, auth gin.HandlerFunc) {
	if auth == nil {
		panic("billing administration requires authentication")
	}
	g := r.Group("/api/v1/billing", auth, h.authorize)
	g.GET("/reservations", h.List)
	g.GET("/reservations/:id", h.Get)
	g.POST("/reservations/:id/reconcile", h.Reconcile)
}
func billingError(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, types.ErrorResponse{Type: "billing_error", Message: message})
}
func (h *BillingHandler) authorize(c *gin.Context) {
	role := currentRole(c)
	id := currentUserID(c)
	if id == nil || *id == 0 || (role != models.RoleSuperAdmin && role != models.RoleTenantAdmin) || (role == models.RoleTenantAdmin && middleware.GetUserTenantID(c) == 0) {
		billingError(c, 403, "Administrator access required")
		return
	}
	c.Next()
}
func billingTenantAllowed(c *gin.Context, tenant uint) bool {
	return middleware.IsSuperAdmin(c) || tenant == middleware.GetUserTenantID(c)
}
func (h *BillingHandler) load(c *gin.Context) (billing.Job, bool, bool) {
	id := c.Param("id")
	if id == "" || len(id) > 128 {
		billingError(c, 400, "Invalid reservation ID")
		return billing.Job{}, false, false
	}
	job, err := h.Store.Get(c.Request.Context(), id)
	durable := false
	if errors.Is(err, billing.ErrMissing) {
		var row billing.Ledger
		err = h.DB.WithContext(c.Request.Context()).First(&row, "reservation_id = ?", id).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			billingError(c, 404, "Reservation not found")
			return job, false, false
		}
		if err == nil {
			durable = true
			job = billing.Job{State: row.State, Reservation: billing.Reservation{ID: row.ReservationID, KeyID: row.VirtualKeyID, TenantID: row.TenantID, Provider: row.Provider, Model: row.Model, Amount: row.ReservedUnits, InputRate: row.InputRate, OutputRate: row.OutputRate}, Settlement: billing.Settlement{Actual: row.ActualUnits, InputTokens: row.InputTokens, OutputTokens: row.OutputTokens, Source: row.Source}}
			if row.ResolutionJSON != "" {
				err = json.Unmarshal([]byte(row.ResolutionJSON), &job.Resolution)
			}
			job.CreatedMillis = row.RequestCreatedMillis
			job.UnresolvedReason = row.UnresolvedReason
			if row.ReportedSettlementJSON != "" && err == nil {
				err = json.Unmarshal([]byte(row.ReportedSettlementJSON), &job.ReportedSettlement)
			}
		}
	}
	if err != nil {
		billingError(c, 503, "Billing state unavailable")
		return job, false, false
	}
	if !billingTenantAllowed(c, job.Reservation.TenantID) {
		billingError(c, 404, "Reservation not found")
		return job, false, false
	}
	return job, durable, true
}
func (h *BillingHandler) List(c *gin.Context) {
	offset, e1 := strconv.ParseInt(c.DefaultQuery("offset", "0"), 10, 64)
	limit, e2 := strconv.ParseInt(c.DefaultQuery("limit", "50"), 10, 64)
	if e1 != nil || e2 != nil || offset < 0 || limit < 1 || limit > 100 {
		billingError(c, 400, "Invalid pagination")
		return
	}
	jobs, err := h.Store.Unresolved(c.Request.Context(), offset, limit)
	if err != nil {
		billingError(c, 503, "Billing state unavailable")
		return
	}
	visible := make([]billing.Job, 0, len(jobs))
	for _, job := range jobs {
		if billingTenantAllowed(c, job.Reservation.TenantID) {
			visible = append(visible, job)
		}
	}
	c.JSON(200, gin.H{"reservations": visible, "next_offset": offset + limit})
}
func (h *BillingHandler) Get(c *gin.Context) {
	job, durable, ok := h.load(c)
	if ok {
		c.JSON(200, gin.H{"reservation": job, "from_ledger": durable})
	}
}
func (h *BillingHandler) Reconcile(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8192)
	var resolution billing.Resolution
	if err := c.ShouldBindJSON(&resolution); err != nil {
		billingError(c, 400, "Invalid resolution body")
		return
	}
	resolution.Actor = fmt.Sprintf("user:%d", *currentUserID(c))
	if strings.TrimSpace(resolution.ID) == "" || len(resolution.ID) > 128 || strings.TrimSpace(resolution.Reason) == "" || len(resolution.Reason) > 1000 || strings.TrimSpace(resolution.Evidence) == "" || len(resolution.Evidence) > 2000 || (resolution.Action != "settle" && resolution.Action != "release") || resolution.InputTokens < 0 || resolution.OutputTokens < 0 {
		billingError(c, 400, "Resolution ID, action, usage, reason and evidence are required")
		return
	}
	job, durable, ok := h.load(c)
	if !ok {
		return
	}
	if durable {
		if job.Resolution == nil || *job.Resolution != resolution {
			billingError(c, 409, "Resolution conflicts with durable ledger")
			return
		}
	} else if err := h.Store.Reconcile(c.Request.Context(), job.Reservation.ID, resolution); err != nil {
		billingError(c, 409, "Resolution rejected; inspect current reservation and evidence")
		return
	}
	c.JSON(200, gin.H{"status": "accepted", "reservation_id": job.Reservation.ID, "resolution_id": resolution.ID})
}
