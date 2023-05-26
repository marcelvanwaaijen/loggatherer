package main

import (
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mvanwaaijen/execpath"
	"gopkg.in/ini.v1"
)

var (
	start     string
	dur       time.Duration
	cfg       *ini.File
	cluster   string
	startTime time.Time
	endTime   time.Time
	compress  bool
	clean     bool
	showver   bool
)

//go:generate genver.exe

func init() {
	var err error
	ep, _ := execpath.Get()
	cfg, err = ini.Load(fmt.Sprintf("%s.ini", ep))
	if err != nil {
		log.Fatalf("[fatal] cannot open ini file: %v", err)
	}
}

func main() {
	var err error

	wd, _ := execpath.GetDir()
	defaultDuration, _ := time.ParseDuration("1h")
	dur = cfg.Section("default").Key("duration").MustDuration(defaultDuration)
	// flag.StringVar(&start, "start", time.Now().Add(-1*dur).UTC().Format("2006-01-02 15:04:05"), "time in UTC (yyyy-MM-dd HH:mm:ss) from when you want to start collecting the logs")
	flag.StringVar(&start, "start", "", "time in UTC (yyyy-MM-dd HH:mm:ss) from when you want to start collecting the logs (default: current UTC time - duration)")
	flag.DurationVar(&dur, "duration", dur, "duration of the period you want to have the logs for (1h = 1 hour, 15m = 15 minutes, etc)")
	flag.StringVar(&cluster, "cluster", cfg.Section("default").Key("cluster").Value(), "cluster to gather logs from")
	flag.BoolVar(&compress, "compress", false, "gzip compress the individual log files")
	flag.BoolVar(&clean, "clean", false, "clean up any log folders for the specified cluster which are older than the specified duration")
	flag.BoolVar(&showver, "version", false, "show version information")
	flag.Parse()

	if showver {
		ShowVersion()
	}

	if clean {
		log.Printf("starting clean-up of logs")
		cleanup()
		log.Print("finished")
		os.Exit(0)
	}
	if len(start) == 0 {
		startTime = time.Now().UTC().Add(-1 * dur)
	} else {
		startTime, err = time.ParseInLocation("2006-01-02 15:04:05", start, time.UTC)
		if err != nil {
			log.Fatalf("[fatal] cannot parse start date: %v", err)
		}
	}
	endTime = startTime.Add(dur)

	var destination string
	if filepath.IsAbs(cfg.Section("default").Key("destination").Value()) {
		destination = cfg.Section("default").Key("destination").Value()
	} else {
		destination = fmt.Sprintf("%s/%s", strings.ReplaceAll(wd, "\\", "/"), cfg.Section("default").Key("destination").Value())
	}
	destination += fmt.Sprintf("/%s/%s-%s", cluster, startTime.Format("20060102T150405Z"), endTime.Format("20060102T150405Z"))

	sect := cfg.Section(cluster)
	var (
		share string
		wg    sync.WaitGroup
	)
	for _, k := range sect.Keys() {
		if k.Name() == "logshare" {
			share = k.MustString("SPSS_DIMENSIONS_LOGS")
			continue
		}
		wg.Add(1)
		go CopyFiles(k.Name(), fmt.Sprintf("//%s/%s", k.Value(), share), destination, &wg)
	}
	wg.Wait()
}

func CopyFiles(server, src, dst string, w *sync.WaitGroup) {
	defer w.Done()
	log.Printf("[info] scanning %s", src)

	_, err := os.Stat(fmt.Sprintf("%s/%s", dst, server))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(fmt.Sprintf("%s/%s", dst, server), 0777); err != nil {
				log.Fatalf("[fatal] error creating destination folder: %v", err)
			}
		} else {
			log.Fatalf("[fatal] error opening destination folder: %v (%v)", err, errors.Is(err.(*os.PathError).Err, os.ErrNotExist))
		}
	}

	sdir, err := os.ReadDir(src)
	if err != nil {
		log.Printf("[error][%s] unable to open %q: %v", server, src, err)
		return
	}

	for _, f := range sdir {
		if !f.IsDir() {
			if finfo, err := f.Info(); err != nil {
				log.Printf("[error][%s] cannot read file info for %q: %v", server, f.Name(), err)
				continue
			} else {
				targetName := finfo.Name()
				if compress {
					targetName += ".gz"
				}
				// log.Printf("[debug][%s] checking %s (m=%s | c=%s)...", server, finfo.Name(), finfo.ModTime().Format("2006-01-02 15:04:05"), time.Unix(0, finfo.Sys().(*syscall.Win32FileAttributeData).CreationTime.Nanoseconds()).Format("2006-01-02 15:04:05"))

				fMod := finfo.ModTime()
				fCreate := time.Unix(0, finfo.Sys().(*syscall.Win32FileAttributeData).CreationTime.Nanoseconds())
				if fMod.After(startTime) && fCreate.Before(endTime) && strings.HasSuffix(finfo.Name(), ".tmp") {
					// log.Printf("[debug][%s] file %s is between %q and %q", server, finfo.Name(), startTime.Format("2006-01-02 15:04:05"), endTime.Format("2006-01-02 15:04:05"))
					s, err := os.Open(fmt.Sprintf("%s/%s", src, f.Name()))
					if err != nil {
						log.Printf("[error][%s] cannot open source file %q: %v", server, f.Name(), err)
						continue
					}
					var (
						d  io.WriteCloser
						zd io.WriteCloser
					)
					if compress {
						zd, err = os.Create(fmt.Sprintf("%s/%s/%s", dst, server, targetName))
						d = gzip.NewWriter(zd)
						if err != nil {
							log.Printf("[error][%s] cannot open destination file %q: %v", server, targetName, err)
							s.Close()
							continue
						}
					} else {
						d, err = os.Create(fmt.Sprintf("%s/%s/%s", dst, server, targetName))
						if err != nil {
							log.Printf("[error][%s] cannot open destination file %q: %v", server, targetName, err)
							s.Close()
							continue
						}
					}
					if err := copyFile(s, d); err != nil {
						log.Printf("[error][%s] cannot copy source to destination %q: %v", server, targetName, err)
						s.Close()
						d.Close()
						if compress {
							zd.Close()
						}
						continue
					}
					s.Close()
					d.Close()
					if compress {
						zd.Close()
					}
					// log.Printf("[debug][%s] setting last modified date on %s to %s...", server, finfo.Name(), fMod.Format("2006-01-02 15:04:05"))
					if err := os.Chtimes(fmt.Sprintf("%s/%s/%s", dst, server, targetName), time.Now(), fMod); err != nil {
						log.Printf("[error][%s] error setting last modified date on %s: %v", server, targetName, err)
					}
				}
			}
		}
	}
	log.Printf("[info] done scanning %s", server)
}

func copyFile(source io.Reader, dest io.Writer) error {
	buf := make([]byte, 1024)
	for {
		n, err := source.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}

		if _, err := dest.Write(buf[:n]); err != nil {
			return err
		}
	}
	return nil
}

func cleanup() {
	var destination string
	wd, _ := execpath.GetDir()

	if filepath.IsAbs(cfg.Section("default").Key("destination").Value()) {
		destination = cfg.Section("default").Key("destination").Value()
	} else {
		destination = fmt.Sprintf("%s/%s", strings.ReplaceAll(wd, "\\", "/"), cfg.Section("default").Key("destination").Value())
	}
	destination += fmt.Sprintf("/%s", cluster)

	entries, err := os.ReadDir(destination)
	if err != nil {
		log.Fatalf("cannot read from folder %q: %v", destination, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			dtParts := strings.Split(entry.Name(), "-")
			if len(entry.Name()) == 33 && len(dtParts) == 2 {
				endT, err := time.Parse("20060102T150405Z", dtParts[1])
				if err != nil {
					continue
				}

				if endT.Before(time.Now().UTC().Add(-1 * dur)) {
					log.Printf("cleaning up %s...", fmt.Sprintf("%s/%s", destination, entry.Name()))
					if err := os.RemoveAll(fmt.Sprintf("%s/%s", destination, entry.Name())); err != nil {
						log.Printf("cannot delete folder %q: %v", fmt.Sprintf("%s/%s", destination, entry.Name()), err)
					}
				}
			}
		}
	}
}
