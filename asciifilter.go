package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// flush to disk every 100k kept lines to balance I/O and memory
	chunkSize = 100_000
)

// isASCII returns true when every byte in the string is within 7‑bit ASCII (0‑127)
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

func main() {
	if len(os.Args) != 3 {
		log.Fatalf("Usage: %s <wordlistFile> <filteredOutput>\n", os.Args[0])
	}

	inputPath := os.Args[1]
	outputPath := os.Args[2]

	//‑‑‑ open files ‑‑‑
	inFile, err := os.Open(inputPath)
	if err != nil {
		log.Fatalf("opening input: %v", err)
	}
	defer inFile.Close()

	outFile, err := os.Create(outputPath)
	if err != nil {
		log.Fatalf("creating output: %v", err)
	}
	defer outFile.Close()

	fi, err := inFile.Stat()
	if err != nil {
		log.Fatalf("stat input: %v", err)
	}
	totalSize := fi.Size() // bytes on disk – used for progress % & ETA

	//‑‑‑ channels and atomics for stats ‑‑‑
	lineCh := make(chan string, 10_000) // read → workers
	keepCh := make(chan string, 10_000) // workers → writer

	var bytesRead int64 // bytes consumed from input (atomic)
	var linesSeen int64 // total lines scanned (atomic)
	var linesKept int64 // lines written to output (atomic)

	//‑‑‑ progress ticker (runs until doneCh closed) ‑‑‑
	doneCh := make(chan struct{})
	go func() {
		start := time.Now()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				br := atomic.LoadInt64(&bytesRead)
				ls := atomic.LoadInt64(&linesSeen)
				lk := atomic.LoadInt64(&linesKept)

				pct := float64(br) / float64(totalSize) * 100
				elapsed := time.Since(start)

				// simple ETA: proportional to processed bytes
				var eta time.Duration
				if pct > 0 {
					eta = time.Duration(float64(elapsed) * (100/pct - 1))
				}

				log.Printf("[progress] %.1f%% — %d/%d MiB | lines read: %d | kept: %d | elapsed: %s | ETA: %s", pct, br>>20, totalSize>>20, ls, lk, elapsed.Truncate(time.Second), eta.Truncate(time.Second))
			case <-doneCh:
				return
			}
		}
	}()

	//‑‑‑ worker pool: ASCII filter ‑‑‑
	workerCount := runtime.NumCPU()
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func() {
			defer wg.Done()
			for line := range lineCh {
				if isASCII(line) {
					atomic.AddInt64(&linesKept, 1)
					keepCh <- line + "\n" // append newline now for faster join in writer
				}
			}
		}()
	}

	//‑‑‑ writer goroutine – batches to disk every chunkSize kept lines ‑‑‑
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		buf := make([]string, 0, chunkSize)
		for s := range keepCh {
			buf = append(buf, s)
			if len(buf) >= chunkSize {
				if _, err := outFile.WriteString(strings.Join(buf, "")); err != nil {
					log.Fatalf("writing output: %v", err)
				}
				buf = buf[:0]
			}
		}
		// flush whatever remains
		if len(buf) > 0 {
			if _, err := outFile.WriteString(strings.Join(buf, "")); err != nil {
				log.Fatalf("writing output: %v", err)
			}
		}
	}()

	//‑‑‑ reader: scan file & feed workers ‑‑‑
	scanner := bufio.NewScanner(inFile)
	// increase max token size to handle very long words/lines (10 MiB)
	scanner.Buffer(make([]byte, 0, 1<<20), 10*1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		atomic.AddInt64(&linesSeen, 1)
		atomic.AddInt64(&bytesRead, int64(len(line)+1)) // +1 for newline byte
		lineCh <- line
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("scanning input: %v", err)
	}

	//‑‑‑ orderly shutdown ‑‑‑
	close(lineCh) // no more work
	wg.Wait()     // workers done
	close(keepCh) // writer can finish
	writerWG.Wait()

	close(doneCh) // stop progress ticker

	fmt.Printf("Finished! Read %d lines, kept %d (%.1f%%). Output saved to %s\n", linesSeen, linesKept, float64(linesKept)/float64(linesSeen)*100, outputPath)
}
