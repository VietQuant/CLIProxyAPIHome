package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

// GetQuotaAutoResetConfig handles GET /quota/auto-reset-config.
func (h *Handler) GetQuotaAutoResetConfig(c *gin.Context) {
	if h == nil || h.repo == nil {
		respondQuotaHTTPError(c, http.StatusNotFound, "QUOTA_AUTO_RESET_UNSUPPORTED", "quota auto reset is not available on this runtime", false)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errGet := h.repo.GetQuotaAutoResetConfig(ctx)
	if errGet != nil {
		respondQuotaHTTPError(c, http.StatusInternalServerError, "QUOTA_AUTO_RESET_READ_FAILED", "failed to read quota auto reset config", true)
		return
	}
	c.JSON(http.StatusOK, record)
}

// UpdateQuotaAutoResetConfig handles PUT /quota/auto-reset-config.
//
// Every field is required rather than patched: these settings decide when an
// irreversible reset credit is spent, so a partial update that silently retained an
// old rule flag would be the wrong default.
func (h *Handler) UpdateQuotaAutoResetConfig(c *gin.Context) {
	if h == nil || h.repo == nil {
		respondQuotaHTTPError(c, http.StatusNotFound, "QUOTA_AUTO_RESET_UNSUPPORTED", "quota auto reset is not available on this runtime", false)
		return
	}
	var body cluster.QuotaAutoResetConfigRecord
	if c.Request == nil || c.Request.Body == nil {
		respondQuotaHTTPError(c, http.StatusBadRequest, "INVALID_BODY", "quota auto reset config body is required", false)
		return
	}
	decoder := json.NewDecoder(c.Request.Body)
	if errDecode := decoder.Decode(&body); errDecode != nil {
		message := "quota auto reset config body must be a JSON object"
		if errors.Is(errDecode, io.EOF) {
			message = "quota auto reset config body is required"
		}
		respondQuotaHTTPError(c, http.StatusBadRequest, "INVALID_BODY", message, false)
		return
	}
	if errValidate := body.Validate(); errValidate != nil {
		respondQuotaHTTPError(c, http.StatusBadRequest, "INVALID_CONFIG", errValidate.Error(), false)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	saved, errSave := h.repo.SaveQuotaAutoResetConfig(ctx, body, quotaAutoResetActor(c))
	if errSave != nil {
		respondQuotaHTTPError(c, http.StatusInternalServerError, "QUOTA_AUTO_RESET_WRITE_FAILED", "failed to save quota auto reset config", true)
		return
	}
	c.JSON(http.StatusOK, saved)
}

// quotaAutoResetActor records who changed the settings when the caller identifies
// itself. It is advisory only and stays empty rather than guessing.
func quotaAutoResetActor(c *gin.Context) string {
	if c == nil {
		return ""
	}
	actor := strings.TrimSpace(c.GetHeader("X-Management-Actor"))
	if len(actor) > 128 {
		return actor[:128]
	}
	return actor
}
