// Command dashboard-preview serves the gpsync web dashboard against a
// ledger COPY with a fake watch controller, so its pages can be looked at
// (and screenshotted at phone widths) without starting the watch engine.
//
// It exists for layout work. The dashboard otherwise only runs inside
// gpsync-tray, which uploads, holds the Windows file lock, and needs a
// graceful quit before every rebuild -- none of which a CSS change should
// require. Nothing here talks to Google: the controller is a stub that
// returns canned live state, and the handler only reads the ledger.
//
// Not built by the Makefile and never deployed.
//
//	GPSYNC_STATE_DIR=/path/to/scratch go run ./cmd/dashboard-preview -addr 127.0.0.1:18080
//
// Point GPSYNC_STATE_DIR at a directory holding a COPY of state.sqlite --
// never the live ~/.gpsync, since page handlers such as Settings can write.
package main

import (
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/dashboard"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/uploader"
)

type stubController struct {
	cfg     config.Config
	running bool
	paused  bool
	start   time.Time
}

func (s *stubController) Config() config.Config         { return s.cfg }
func (s *stubController) SetConfig(cfg config.Config)   { s.cfg = cfg }
func (s *stubController) IsRunning() bool               { return s.running }
func (s *stubController) Start()                        { s.running = true }
func (s *stubController) Stop()                         { s.running = false }
func (s *stubController) Scanning() bool                { return false }
func (s *stubController) FileProgress() (int, int, int) { return 23, 6, 50 }
func (s *stubController) FolderProgress() (int, int)    { return 0, 0 }
func (s *stubController) CurrentFolder() string         { return "" }
func (s *stubController) RunProgress() (int64, int64, time.Time) {
	// Must exceed the in-flight totals below: a screenshot showing one
	// file past the whole run is how the published README image went
	// out wrong.
	return 4200 << 20, 11500 << 20, s.start
}
func (s *stubController) RunPausedFor() time.Duration       { return 0 }
func (s *stubController) RequestRetryNow()                  {}
func (s *stubController) CancelFile(path string) bool       { return true }
func (s *stubController) SyncFolderNow(folder string) error { return nil }

// InFlightFiles returns a realistic spread: long and short names, partial
// and complete, so row truncation and the progress bar both get exercised.
func (s *stubController) InFlightFiles() map[string]uploader.InFlightFile {
	return map[string]uploader.InFlightFile{
		`C:\Photos\2022\2022_07 Summer Trip\20220724_222219.mp4`:   {Sent: 812 << 20, Total: 2192 << 20},
		`C:\Photos\2022\2022_07 Theme Park\20220729_182935.mp4`:    {Sent: 2100 << 20, Total: 3317 << 20},
		`C:\Photos\2022\2022_10 City Break\20221028_141233.jpg`:    {Sent: 4 << 20, Total: 4 << 20},
		`C:\Photos\2022\2022_10 Mountains\IMG-20221027-WA0031.jpg`: {Sent: 1 << 19, Total: 2 << 20},
		`C:\Photos\2022\2022_03 Race Weekend\20220319_183224.mp4`:  {Sent: 96 << 20, Total: 1039 << 20},
		`C:\Photos\2022\2022_09 Birthday\20220905_203114.mp4`:      {Sent: 20 << 20, Total: 61 << 20},
	}
}

func (s *stubController) RecentEvents() []uploader.ProgressEvent {
	return []uploader.ProgressEvent{
		{LastFile: `C:\Photos\2022\2022_10 Sightseeing\20221005_101112.jpg`, LastOK: true, LastSize: 5 << 20},
		{LastFile: `C:\Photos\2022\2022_10 Boat Trip\20221005_174455.mp4`, LastOK: false, LastErrorMessage: "Quota exceeded for quota 'concurrent write request' of service 'photoslibrary.googleapis.com'."},
		{LastFile: `C:\Photos\2022\2022_10 Museum Day\20221015_112233.jpg`, LastOK: true, LastSize: 3 << 20},
		{LastFile: `C:\Photos\2022\2022_09 Get-together\20220918_090001.mp4`, LastCancelled: true},
		{LastFile: `C:\Photos\2022\2022_10 Weekend - Hotel\IMG-20221008-WA0004.jpg`, LastOK: true, LastSize: 1 << 20},
	}
}

func (s *stubController) CurrentBackoff() *uploader.BackoffStatus {
	if !s.paused {
		return nil
	}
	return &uploader.BackoffStatus{
		Active:         true,
		Reason:         "Quota exceeded for quota 'concurrent write request' of service 'photoslibrary.googleapis.com'.",
		Rung:           2,
		TotalRungs:     5,
		RungWait:       10 * time.Minute,
		RemainingWait:  7*time.Minute + 12*time.Second,
		Concurrency:    1,
		MaxConcurrency: 6,
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	paused := flag.Bool("paused", false, "render the Status page mid-backoff")
	flag.Parse()

	if os.Getenv("GPSYNC_STATE_DIR") == "" {
		log.Fatal("GPSYNC_STATE_DIR must point at a directory holding a COPY of state.sqlite -- refusing to open the live ledger")
	}
	db, err := statedb.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	cfg := config.Defaults()
	cfg.Theme = config.ThemeDark
	cfg.SourceFolders = []string{`C:\Photos`}
	ctrl := &stubController{cfg: cfg, running: true, paused: *paused, start: time.Now().Add(-47 * time.Minute)}

	mux := http.NewServeMux()
	// /__frame hosts a dashboard page inside an iframe of an exact CSS size.
	// Headless Chromium on Windows enforces a ~500px minimum window width,
	// so --window-size cannot produce a phone viewport directly: a request
	// for 384x832 reports innerWidth 492. An iframe's viewport is exactly
	// the box it is given, and media queries plus innerHeight follow it.
	//	/__frame?w=384&h=832&src=/statistics
	mux.HandleFunc("/__frame", func(w http.ResponseWriter, r *http.Request) {
		width, _ := strconv.Atoi(r.URL.Query().Get("w"))
		height, _ := strconv.Atoi(r.URL.Query().Get("h"))
		if width <= 0 || height <= 0 {
			http.Error(w, "w and h are required", http.StatusBadRequest)
			return
		}
		src := r.URL.Query().Get("src")
		if src == "" || src[0] != '/' {
			src = "/"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><body style="margin:0;background:#000">`+
			`<iframe src="%s" style="display:block;width:%dpx;height:%dpx;border:0"></iframe>`,
			html.EscapeString(src), width, height)
		// debug=1 appends layout measurements of the framed Status page's
		// two live lists, readable with Edge's --dump-dom. The row-capacity
		// logic there reads clientHeight and row pitch back from the DOM,
		// and a screenshot shows only its outcome, not the numbers that
		// produced it. Same origin, so the host can read the frame's DOM.
		if r.URL.Query().Get("debug") == "1" {
			fmt.Fprint(w, `<script>setTimeout(function(){`+
				`var f=document.querySelector('iframe'),fw=f.contentWindow,d=f.contentDocument,out={innerHeight:fw.innerHeight};`+
				`['#status-uploading .live-list','#status-recent .live-list'].forEach(function(sel){`+
				`var el=d.querySelector(sel);if(!el){out[sel]=null;return;}var rows=el.querySelectorAll('.file-row');`+
				`out[sel]={clientHeight:el.clientHeight,scrollHeight:el.scrollHeight,rows:rows.length,`+
				`rowH:rows[0]?rows[0].offsetHeight:null,pitch:rows.length>1?rows[1].offsetTop-rows[0].offsetTop:null,`+
				`cardH:el.parentElement.offsetHeight,display:fw.getComputedStyle(el).display,flex:fw.getComputedStyle(el).flex};});`+
				`try{out.upCap=fw.measureUploadingCapacity();out.recentCap=fw.measureRecentCapacity();}catch(e){out.err=String(e);}`+
				`var p=document.createElement('pre');p.id='dbg';p.textContent=JSON.stringify(out);document.body.appendChild(p);`+
				`},4000);</script>`)
		}
		fmt.Fprint(w, `</body></html>`)
	})
	// /__probe prints the viewport it was rendered in, to prove the frame
	// really is the size asked for before trusting any screenshot of it.
	mux.HandleFunc("/__probe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><head><meta name="viewport" content="width=device-width, initial-scale=1"></head>`+
			`<body style="margin:0;background:#123;color:#fff;font:32px sans-serif;padding:16px"><div id=v></div>`+
			`<script>document.getElementById('v').textContent=innerWidth+'x'+innerHeight+' dpr '+devicePixelRatio`+
			`+' '+(matchMedia('(max-width: 600px)').matches?'phone-mq':'wide-mq')</script></body></html>`)
	})
	mux.Handle("/", dashboard.Handler(db, ctrl, dashboard.Options{AppName: "GPhotos Sync"}))

	fmt.Printf("dashboard preview on http://%s (ledger: %s)\n", *addr, statedb.StateDBPath)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Fatal(srv.ListenAndServe())
}
