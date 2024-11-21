package main

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/neokofg/go-pet-webrtc/internal/app/entities"
	"html/template"
	"net/http"
	"runtime"
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	startHTTPServer()
}

func startHTTPServer() {
	router := gin.Default()

	tmpl := template.Must(template.ParseFiles("templates/index.html"))

	router.GET("/", func(c *gin.Context) {
		tmpl.Execute(c.Writer, nil)
	})

	router.POST("/join-room", func(c *gin.Context) {
		c.String(http.StatusOK, `<div class="video-container">
            <video id="localVideo" autoplay playsinline muted></video>
            <div class="video-label">You</div>
        </div>`)
	})

	router.POST("/leave-room", func(c *gin.Context) {
		c.String(http.StatusOK, `<div class="video-grid" id="videoGrid"></div>`)
	})

	sfu := entities.NewSFUServer()
	router.GET("/ws", sfu.HandleWebSocket)

	fmt.Println("Starting HTTP Server on :8080")
	router.Run("0.0.0.0:8080")
}
