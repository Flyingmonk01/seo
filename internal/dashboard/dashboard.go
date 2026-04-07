package dashboard

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed static
var staticFiles embed.FS

// Register mounts the dashboard UI at /dashboard
func Register(r *gin.Engine) {
	sub, _ := fs.Sub(staticFiles, "static")
	r.GET("/dashboard", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/dashboard/")
	})
	r.StaticFS("/dashboard/", http.FS(sub))
}
