package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"context"

	"golang.org/x/sync/errgroup"
)

var (
	userPass = flag.String("github_user_pass",
		"",
		"If non-empty, a user:password string for HTTP basic authentication. See https://github.com/settings/tokens")
)

// Git commit hash of https://github.com/raspberrypi/rpi-eeprom to take EEPROM
// updates from.
const eepromRef = "4c5aebdb200bc9a2ffd2a0158efffb9603c33be7"

type contentEntry struct {
	Name        string `json:"name"`
	Sha         string `json:"sha"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"download_url"`
}

func authenticate(req *http.Request) {
	if *userPass != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(*userPass)))
	}
}

func githubContents(url string) (map[string]contentEntry, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	authenticate(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code: got %d, want %d (body: %s)", got, want, string(body))
	}
	var contents []contentEntry
	if err := json.NewDecoder(resp.Body).Decode(&contents); err != nil {
		return nil, err
	}
	result := make(map[string]contentEntry, len(contents))
	for _, c := range contents {
		result[c.Name] = c
	}
	return result, nil
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if *userPass == "" {
		if fromEnv := os.Getenv("GITHUB_USER") + ":" + os.Getenv("GITHUB_AUTH_TOKEN"); fromEnv != "" {
			*userPass = fromEnv
		}
	}

	eepromFiles, err := filepath.Glob("*.bin")
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("eepromFiles = %v", eepromFiles)

	// Calculate the git blob hash of each file
	var (
		firmwareHashesMu sync.Mutex
		firmwareHashes   = make(map[string]string, len(eepromFiles))
	)
	var eg errgroup.Group
	for _, path := range eepromFiles {
		path := path // copy
		eg.Go(func() error {
			hash := sha1.New()
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			st, err := f.Stat()
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(hash, "blob %d\x00", st.Size()); err != nil {
				return err
			}
			if _, err := io.Copy(hash, f); err != nil {
				return err
			}

			firmwareHashesMu.Lock()
			defer firmwareHashesMu.Unlock()
			firmwareHashes[filepath.Base(path)] = fmt.Sprintf("%x", hash.Sum(nil))
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		log.Fatal(err)
	}

	contents, err := githubContents("https://api.github.com/repos/raspberrypi/rpi-eeprom/contents/firmware-2711/latest?ref=" + eepromRef)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("contents: %v", contents)

	ctx, canc := context.WithDeadline(context.Background(), time.Now().Add(1*time.Minute))
	defer canc()
	deg, ctx := errgroup.WithContext(ctx)
	for path, githubContent := range contents {
		fn := filepath.Base(path)
		localHash, ok := firmwareHashes[fn]
		if ok && localHash == githubContent.Sha {
			delete(firmwareHashes, fn)
			continue // up to date
		}
		delete(firmwareHashes, fn)
		// not found, or not up to date
		log.Printf("getting %s (local %s, GitHub %s)", fn, localHash, githubContent.Sha)
		githubContent, path := githubContent, path // copy
		deg.Go(func() error {
			log.Printf("fetching %v", githubContent)
			req, err := http.NewRequest(http.MethodGet, githubContent.DownloadURL, nil)
			if err != nil {
				return err
			}
			authenticate(req)
			req.Header.Set("Accept", "application/vnd.github.v3.raw")

			resp, err := http.DefaultClient.Do(req.WithContext(ctx))
			if err != nil {
				return err
			}

			f, err := os.Create(path)
			if err != nil {
				return err
			}
			defer f.Close()

			if _, err := io.Copy(f, resp.Body); err != nil {
				return err
			}

			return f.Close()
		})
	}
	if err := deg.Wait(); err != nil {
		log.Fatal(err)
	}
	for leftover := range firmwareHashes {
		if err := os.Remove(leftover); err != nil {
			log.Fatalf("removing left-over file: %v", err)
		}
	}
}
