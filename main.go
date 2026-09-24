package main

import (
	"embed"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"ducktective/internal/app"
)

//go:embed all:frontend/dist
var assets embed.FS

// DUCKTECTIVE_PPROF=1 opts into a localhost-only pprof server for diagnosing memory/goroutine issues — see internal/app/CLAUDE.md.
func maybeStartPprof() {
	if os.Getenv("DUCKTECTIVE_PPROF") == "" {
		return
	}
	go func() {
		log.Println("[pprof] listening on http://localhost:6061/debug/pprof/")
		log.Println("[pprof]", http.ListenAndServe("localhost:6061", nil))
	}()
}

func main() {
	maybeStartPprof()
	a := app.New()

	err := wails.Run(&options.App{
		Title:  "Ducktective",
		Width:  1200,
		Height: 800,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 15, G: 22, B: 32, A: 1},
		OnStartup:        a.Startup,
		OnShutdown:       a.Shutdown,
		Bind: []any{
			a,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
