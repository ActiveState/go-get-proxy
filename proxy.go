package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const modtimeFile = ".go-get-proxy-last"

var (
	listen = flag.String("listen", ":8080", "port, ip:port, or 'envfd:NAME' to listen on")
)

var (
	pendingMu sync.Mutex
	pending   = make(map[string]chan bool)
)

func proxy(w http.ResponseWriter, r *http.Request) {
	upath := r.URL.Path
	switch upath {
	case "/favicon.ico", "/robots.txt":
		// TODO(brafitz): handle
		return
	}
	if len(upath) < 2 {
		fmt.Fprintf(w, "<html><body>go get proxy</body></html>")
		return
	}
	if path.Clean(upath) != upath {
		log.Printf("invalid requested path %q", upath)
		http.Error(w, "invalid path", 500)
		return
	}

	pkg := upath[1:]

	dir, file := path.Split(upath)
	if strings.HasSuffix(file, ".go") {
		pkg = dir[1 : len(dir)-1]
	} else {
		file = ""
	}

	path, err := getPackage(pkg)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(500)
		fmt.Fprintf(w, err.Error())
		return
	}

	switch {
	case file == "":
		// Tar mode.
		w.Header().Set("Content-Type", "application/x-tar")
		err = makeTar(w, path)
		if err != nil {
			log.Printf("Error generating tar of %q: %v", path, err)
		}
		return
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		f, err := os.Open(filepath.Join(path, file))
		if err != nil {
			w.WriteHeader(500)
			fmt.Fprintf(w, err.Error())
			return
		}
		defer f.Close()
		io.Copy(w, f)
	}
}

var goPathSrc = filepath.Join(os.Getenv("GOPATH"), "src")

const newEnough = 1 * time.Minute

func isNewEnough(dir string) (ret bool) {
	for len(dir) > len(goPathSrc) {
		if fi, err := os.Stat(filepath.Join(dir, modtimeFile)); err == nil {
			if time.Now().Sub(fi.ModTime()) < newEnough {
				log.Printf("Dir %s is new enough.", dir)
				return true
			}
		}
		dir = filepath.Join(dir, "..")
	}
	return false
}

func getPackage(pkg string) (pkgPath string, err error) {
	pkgPath = filepath.Join(goPathSrc, filepath.FromSlash(pkg))
	if isNewEnough(pkgPath) {
		return
	}

	// Only allow a package to be fetched once at a time.
	// TODO(bradfitz): this isn't perfect synchronization. we're
	// only protecting the top level. the go get tool will go
	// fetch dependencies that we don't see here.
	pendingMu.Lock()
	c, ok := pending[pkg]
	if !ok {
		c = make(chan bool, 1)
		pending[pkg] = c
	}
	pendingMu.Unlock()
	c <- true // blocks until buffer size of 1 is free
	defer func() { <-c }()

	log.Printf("Getting package %q...", pkg)
	cmd := exec.Command("go", "get", "-u", "-d", pkg)

	out, err := cmd.CombinedOutput()
	if err != nil {
		// TODO: set a global "last failure time" for this package (or up a level),
		// so some expensive failure can't happen often quickly.
		log.Printf("Get of package %q failed: %v; output: %s", pkg, err, out)
		return "", fmt.Errorf("Error running go get for package %q: %v\n\nOutput:\n%s", pkg, err, out)
	}

	log.Printf("Fetched package %q", pkg)

	// Figure out where its root is. The root is the highest level that still has
	// a ".vcs" subdirectory.
	root := pkgPath
	svnSeen := false
	for checkDir := root; ; {
		dirHas := func(vcsDir string) bool {
			fi, err := os.Stat(filepath.Join(checkDir, vcsDir))
			return err == nil && fi.IsDir()
		}
		if dirHas(".svn") {
			// Keep going up until we *don't* see an .svn directory.
			svnSeen = true
		} else if svnSeen {
			break
		}
		root = checkDir
		if dirHas(".hg") || dirHas(".git") || dirHas(".bzr") {
			break
		}
		checkDir = filepath.Join(checkDir, "..")
		if checkDir == root {
			// No change?
			return "", errors.New("confused; vcs file in root?")
		}
	}

	log.Printf("root of %q is: %q", pkg, root)
	filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || !fi.IsDir() {
			return nil
		}
		switch filepath.Base(path) {
		case ".svn", ".hg", ".git", ".bzr":
			return filepath.SkipDir
		}
		tf := filepath.Join(path, modtimeFile)
		touchFile(tf)
		return nil
	})

	return pkgPath, nil
}

func touchFile(name string) {
	os.Remove(name)
	f, err := os.Create(name)
	if err == nil {
		f.Close()
	}
}

func main() {
	flag.Parse()

	var ln net.Listener
	addr := *listen
	if strings.HasPrefix(addr, "envfd:") {
		name := addr[len("envfd:"):]
		fdstr := os.Getenv("RUNSIT_PORTFD_" + name)
		if fdstr == "" {
			log.Fatalf("didn't find named runsit port named %q in environment", name)
		}
		fdnum, err := strconv.Atoi(fdstr)
		if err != nil {
			log.Fatalf("bogus port number %q in environment: %v", fdstr, err)
		}
		ln, err = net.FileListener(os.NewFile(uintptr(fdnum), "fd"))
		if err != nil {
			log.Fatal(err)
		}
	} else {
		if !strings.Contains(addr, ":") {
			addr = ":" + addr
		}
		var err error
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("Listen on %q: %v", addr, err)
		}
	}
	s := &http.Server{
		Handler: http.HandlerFunc(proxy),
	}
	log.Printf("Listened on %q; starting.", addr)
	err := s.Serve(ln)
	log.Fatalf("Serve error: %v", err)
}
