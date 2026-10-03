package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	webp "github.com/SeriousBug/webp-go-pure/std"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
)

const (
	maxUploadBytes = 25 << 20
	maxZipFileSize = 50 << 20
	maxPixels      = 40_000_000
	maxBatchBytes  = 1 << 30
	webpQuality    = 90
	webpEffort     = 6
)

//go:embed web/index.html
var webFiles embed.FS

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", serveIndex)
	mux.HandleFunc("POST /convert", convertImage)
	mux.HandleFunc("POST /download-all", downloadAll)

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("OMIGo is listening at http://%s", addr)
	log.Fatal(server.ListenAndServe())
}

func downloadAll(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBatchBytes+(16<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "The ZIP request is too large or invalid. The maximum total size is 1 GB.", http.StatusRequestEntityTooLarge)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	files := r.MultipartForm.File["images"]
	if len(files) == 0 {
		http.Error(w, "Upload at least one converted image.", http.StatusBadRequest)
		return
	}

	var total int64
	for _, file := range files {
		if file.Size <= 0 || file.Size > maxZipFileSize {
			http.Error(w, "Each converted image must be 50 MB or smaller.", http.StatusRequestEntityTooLarge)
			return
		}
		total += file.Size
	}
	if total > maxBatchBytes {
		http.Error(w, "The ZIP is larger than the 1 GB limit.", http.StatusRequestEntityTooLarge)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="omigo-images.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	archive := zip.NewWriter(w)
	names := make(map[string]int, len(files))
	for i, header := range files {
		base := strings.TrimSuffix(filepath.Base(header.Filename), filepath.Ext(header.Filename))
		base = safeFilename(base)
		if base == "" {
			base = fmt.Sprintf("image-%02d", i+1)
		}
		names[base]++
		if names[base] > 1 {
			base = fmt.Sprintf("%s-%d", base, names[base])
		}

		entry, err := archive.Create(base + ".webp")
		if err != nil {
			log.Printf("creating a ZIP entry: %v", err)
			_ = archive.Close()
			return
		}
		file, err := header.Open()
		if err != nil {
			log.Printf("reading an image for the ZIP: %v", err)
			_ = archive.Close()
			return
		}
		_, copyErr := io.Copy(entry, io.LimitReader(file, maxZipFileSize+1))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			log.Printf("writing an image to the ZIP: %v %v", copyErr, closeErr)
			_ = archive.Close()
			return
		}
	}
	if err := archive.Close(); err != nil {
		log.Printf("finalizing the ZIP: %v", err)
	}
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, mustOpenIndex()); err != nil {
		log.Printf("serving the home page: %v", err)
	}
}

func mustOpenIndex() io.Reader {
	data, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		return strings.NewReader("Could not load the application interface.")
	}
	return bytes.NewReader(data)
}

func convertImage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		http.Error(w, "The file is larger than 25 MB or the upload is invalid.", http.StatusRequestEntityTooLarge)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Upload an image in the image field.", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if header.Size > maxUploadBytes {
		http.Error(w, "The file must be 25 MB or smaller.", http.StatusRequestEntityTooLarge)
		return
	}

	data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		http.Error(w, "Could not read the uploaded file.", http.StatusBadRequest)
		return
	}
	if len(data) == 0 || len(data) > maxUploadBytes {
		http.Error(w, "The file is empty or larger than 25 MB.", http.StatusRequestEntityTooLarge)
		return
	}

	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		http.Error(w, "Invalid or unsupported format. Use JPG, PNG, GIF, WebP, BMP, or TIFF.", http.StatusUnsupportedMediaType)
		return
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxPixels {
		http.Error(w, "The image must be 40 megapixels or smaller.", http.StatusRequestEntityTooLarge)
		return
	}

	img, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format {
		http.Error(w, "Could not decode the image.", http.StatusUnprocessableEntity)
		return
	}

	var output bytes.Buffer
	if err := webp.Encode(&output, img, &webp.Options{Quality: webpQuality, Effort: webpEffort}); err != nil {
		http.Error(w, "Could not convert the image to WebP.", http.StatusInternalServerError)
		return
	}

	filename := strings.TrimSuffix(filepath.Base(header.Filename), filepath.Ext(header.Filename))
	filename = safeFilename(filename)
	if filename == "" {
		filename = "image"
	}

	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename+".webp"))
	w.Header().Set("Content-Length", fmt.Sprint(output.Len()))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(output.Bytes())
}

func safeFilename(name string) string {
	name = strings.TrimSpace(name)
	var result strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			result.WriteRune(r)
		} else if r == ' ' {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-_")
}
