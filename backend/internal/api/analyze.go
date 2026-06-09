// analyze.go implements ad-hoc analysis of an uploaded state file (no stored
// source). Useful for one-off inspection of a `.tfstate` you have on hand.
package api

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
)

const maxUploadBytes = 64 << 20 // 64 MiB

// AnalyzeUploadHandler analyzes a posted state file without persisting it.
// POST /api/v1/analyze — body is the raw .tfstate, or a multipart form field "file".
func AnalyzeUploadHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		data, err := readUpload(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		a, err := analyzer.Analyze(data)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		resources, err := analyzer.ListResources(data)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"analysis": a, "resources": resources})
	}
}

func readUpload(c *gin.Context) ([]byte, error) {
	if fh, err := c.FormFile("file"); err == nil {
		f, openErr := fh.Open()
		if openErr != nil {
			return nil, openErr
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(io.LimitReader(f, maxUploadBytes))
	}
	return io.ReadAll(io.LimitReader(c.Request.Body, maxUploadBytes))
}
