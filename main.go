package main

import (
	"bytes"
	"context"
	"flag"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/inspector"
	"github.com/chromedp/cdproto/profiler"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

func main() {
	logger := log.New(os.Stderr, "[wasmbrowsertest]: ", log.LstdFlags|log.Lshortfile)
	if len(os.Args) < 2 {
		logger.Fatal("Please pass a wasm file as a parameter")
	}

	initFlags()

	wasmFile := os.Args[1]
	ext := path.Ext(wasmFile)
	// net/http code does not take js/wasm path if it is a .test binary.
	if ext == ".test" {
		wasmFile = strings.Replace(wasmFile, ext, ".wasm", -1)
		err := os.Rename(os.Args[1], wasmFile)
		if err != nil {
			logger.Fatal(err)
		}
		defer os.Rename(wasmFile, os.Args[1])
		os.Args[1] = wasmFile
	}
	// We create a copy of the args to pass to NewWASMServer, because flag.Parse needs the
	// 2nd argument (the binary name) removed before being called.
	// This is an effect of "go test" passing all the arguments _after_ the binary name.
	argsCopy := append([]string(nil), os.Args...)
	// Remove the 2nd argument.
	os.Args = append(os.Args[:1], os.Args[2:]...)
	flag.Parse()

	// Need to generate a random port every time for tests in parallel to run.
	l, err := net.Listen("tcp", "localhost:")
	if err != nil {
		logger.Fatal(err)
	}
	tcpL, ok := l.(*net.TCPListener)
	if !ok {
		logger.Fatal("net.Listen did not return a TCPListener")
	}
	_, port, err := net.SplitHostPort(tcpL.Addr().String())
	if err != nil {
		logger.Fatal(err)
	}

	// Setup web server.
	var handler http.Handler
	if webRoot := os.Getenv("WASM_SERVER_ROOT"); webRoot != "" {
		// use a simple web server with no passed in arguments and no logger
		//handler = http.FileServer(http.Dir(webRoot))
		handler = simpleWebServerReturned(webRoot)
	} else {
		handler, err = NewWASMServer(wasmFile, filterCPUProfile(argsCopy[1:]), logger)
		if err != nil {
			logger.Fatal(err)
		}
	}
	httpServer := &http.Server{
		Handler: handler,
	}

	opts := chromedp.DefaultExecAllocatorOptions[:]
	if os.Getenv("WASM_HEADLESS") == "off" {
		opts = append(opts,
			chromedp.Flag("headless", false),
		)
	}

	// WSL needs the GPU disabled. See issue #10
	if runtime.GOOS == "linux" && isWSL() {
		opts = append(opts,
			chromedp.DisableGPU,
		)
	}

	// create chrome instance
	allocCtx, cancelAllocCtx := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAllocCtx()
	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	defer cancelCtx()

	done := make(chan struct{})
	go func() {
		err = httpServer.Serve(l)
		if err != http.ErrServerClosed {
			logger.Println(err)
		}
		done <- struct{}{}
	}()

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		handleEvent(ctx, ev, httpServer, done, logger)
	})

	exitCode := 0
	tasks := []chromedp.Action{
		chromedp.Navigate(`http://localhost:` + port),
		chromedp.WaitEnabled(`#wasm_done`),
		chromedp.Evaluate(`exitCode;`, &exitCode),
	}
	if *cpuProfile != "" {
		// Prepend and append profiling tasks
		tasks = append([]chromedp.Action{
			profiler.Enable(),
			profiler.Start(),
		}, tasks...)
		tasks = append(tasks, chromedp.ActionFunc(func(ctx context.Context) error {
			profile, err := profiler.Stop().Do(ctx)
			if err != nil {
				return err
			}
			outF, err := os.Create(*cpuProfile)
			if err != nil {
				return err
			}
			defer func() {
				err = outF.Close()
				if err != nil {
					logger.Println(err)
				}
			}()

			funcMap, err := getFuncMap(wasmFile)
			if err != nil {
				return err
			}

			return WriteProfile(profile, outF, funcMap)
		}))
	}

	err = chromedp.Run(ctx, tasks...)
	if err != nil {
		logger.Println(err)
	}
	cleanupAndTerminate(ctx, exitCode, httpServer, done, logger)
}

// cleanupAndTerminate tries to clean up the http server and then terminates this program
func cleanupAndTerminate( chromedpContext context.Context,exitCode int, httpServer *http.Server, done chan struct{}, logger *log.Logger) {
	if exitCode != 0 {
		defer os.Exit(1)
	}
	// create a timeout
	ctx, cancelHttpServerCtx := context.WithTimeout(chromedpContext, 5*time.Second)
	defer cancelHttpServerCtx()
	// Close shop
	err := httpServer.Shutdown(ctx)
	if err != nil {
		logger.Println(err)
	}
	<-done
}

// filterCPUProfile removes the cpuprofile argument if passed.
// CPUProfile is taken from the chromedp driver.
// So it is valid to pass such an argument, but the wasm binary will throw an error
// since file i/o is not supported inside the browser.
func filterCPUProfile(args []string) []string {
	tmp := args[:0]
	for _, x := range args {
		if strings.Contains(x, "test.cpuprofile") {
			continue
		}
		tmp = append(tmp, x)
	}
	return tmp
}

// handleEvent responds to different events from the browser and takes
// appropriate action.
func handleEvent(ctx context.Context, ev interface{}, httpServer *http.Server, done chan struct{}, logger *log.Logger) {
	switch ev := ev.(type) {
	case *cdpruntime.EventConsoleAPICalled:
		// Print the full structure for transparency
		jsonBytes, err := ev.MarshalJSON()
		if err != nil {
			logger.Print(err)
			cleanupAndTerminate(ctx, 1, httpServer, done, logger)
		}
		if ev.Type == cdpruntime.APITypeError {
			// special case which can mean the WASM program never initialized
			logger.Printf("fatal error while trying to run tests: %v\n", string(jsonBytes))
			cleanupAndTerminate(ctx, 1, httpServer, done, logger)
		}
		logger.Printf("%v\n", string(jsonBytes))
	case *cdpruntime.EventExceptionThrown:
		if ev.ExceptionDetails != nil && ev.ExceptionDetails.Exception != nil {
			logger.Printf("%s\n", ev.ExceptionDetails.Exception.Description)
		}
	case *target.EventTargetCrashed:
		logger.Printf("target crashed: status: %s, error code:%d\n", ev.Status, ev.ErrorCode)
		err := chromedp.Cancel(ctx)
		if err != nil {
			logger.Printf("error in cancelling context: %v\n", err)
		}
	case *inspector.EventDetached:
		logger.Println("inspector detached: ", ev.Reason)
		err := chromedp.Cancel(ctx)
		if err != nil {
			logger.Printf("error in cancelling context: %v\n", err)
		}
	}
}

// TODO: remove this?
type simpleWebServer struct {
	directory string
	fileServer http.Handler
}

func (ws *simpleWebServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if ws.directory == "" {
		log.Fatal("ws.directory was null")
	}
	if ws.fileServer == nil {
		log.Fatal("ws.fileServer was nil")
	}
	if path.Ext(r.URL.Path) == ".wasm" {
		// special case?
		f, err := os.Open(ws.directory + "/" + r.URL.Path)
		if err != nil {
			log.Fatal(err)
			return
		}
		defer func() {
			err := f.Close()
			if err != nil {
				log.Fatal(err)
			}
		}()
		http.ServeContent(w, r, r.URL.Path, time.Now(), f)
	} else {
		ws.fileServer.ServeHTTP(w, r)
	}
}

func simpleWebServerReturned(directory string) http.Handler {
	srv := &simpleWebServer{
		directory:  directory,
		fileServer: http.FileServer(http.Dir(directory)),
	}
	return srv
}

// isWSL returns true if the OS is WSL, false otherwise
// This method of checking for WSL has worked since mid 2016
// https://github.com/microsoft/WSL/issues/423#issuecomment-328526847
func isWSL() bool {
	b, _ := ioutil.ReadFile("/proc/sys/kernel/osrelease")
	// if there was an error opening the file it must not be WSL, so ignore the error
	return bytes.Contains(b, []byte("Microsoft"))
}
