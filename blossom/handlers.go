package blossom

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/liamg/magic"
	"github.com/nbd-wtf/go-nostr"
)

func (bs BlossomServer) handleUploadCheck(w http.ResponseWriter, r *http.Request) {
	auth, err := readAuthorization(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	if auth != nil {
		if auth.Tags.GetFirst([]string{"t", "upload"}) == nil {
			http.Error(w, "invalid Authorization event \"t\" tag", 403)
			return
		}
	}

	mimetype := r.Header.Get("X-Content-Type")
	exts, _ := mime.ExtensionsByType(mimetype)
	var ext string
	if len(exts) > 0 {
		ext = exts[0]
	}

	// get the file size from the incoming header
	size, _ := strconv.Atoi(r.Header.Get("X-Content-Length"))

	for _, rb := range bs.RejectUpload {
		reject, reason, code := rb(r.Context(), auth, size, ext)
		if reject {
			http.Error(w, reason, code)
			return
		}
	}
}

func (bs BlossomServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	auth, err := readAuthorization(r)
	if err != nil {
		http.Error(w, "invalid Authorization: "+err.Error(), 400)
		return
	}

	if auth != nil {
		if auth.Tags.GetFirst([]string{"t", "upload"}) == nil {
			http.Error(w, "invalid Authorization event \"t\" tag", 403)
			return
		}
	}

	// get the file size from the incoming header
	size, _ := strconv.Atoi(r.Header.Get("Content-Length"))

	// read first bytes of upload so we can find out the filetype
	b := make([]byte, 50, size)
	if _, err = r.Body.Read(b); err != nil {
		http.Error(w, "failed to read initial bytes of upload body: "+err.Error(), 400)
		return
	}
	var ext string
	if ft, _ := magic.Lookup(b); ft != nil {
		ext = "." + ft.Extension
	} else {
		// if we can't find, use the filetype given by the upload header
		mimetype := r.Header.Get("Content-Type")
		if exts, _ := mime.ExtensionsByType(mimetype); len(exts) > 0 {
			ext = exts[0]
		}
	}

	// run the reject hooks
	for _, ru := range bs.RejectUpload {
		reject, reason, code := ru(r.Context(), auth, size, ext)
		if reject {
			http.Error(w, reason, code)
			return
		}
	}

	// if it passes then we have to read the entire thing into memory so we can compute the sha256
	for {
		var n int
		n, err = r.Body.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
		if len(b) == cap(b) {
			// add more capacity (let append pick how much)
			// if Content-Length was correct we shouldn't reach this
			b = append(b, 0)[:len(b)]
		}
	}
	if err != nil {
		http.Error(w, "failed to read upload body: "+err.Error(), 400)
		return
	}

	hash := sha256.Sum256(b)
	hhash := hex.EncodeToString(hash[:])

	// keep track of the blob descriptor
	bd := BlobDescriptor{
		URL:      bs.ServiceURL + "/" + hhash + ext,
		SHA256:   hhash,
		Size:     len(b),
		Type:     mime.TypeByExtension(ext),
		Uploaded: nostr.Now(),
	}
	if err := bs.Store.Keep(r.Context(), bd, auth.PubKey); err != nil {
		http.Error(w, "failed to save event: "+err.Error(), 400)
		return
	}

	// save actual blob
	for _, sb := range bs.StoreBlob {
		if err := sb(r.Context(), hhash, b); err != nil {
			http.Error(w, "failed to save: "+err.Error(), 500)
			return
		}
	}

	// return response
	json.NewEncoder(w).Encode(bd)
}

func (bs BlossomServer) handleGetBlob(w http.ResponseWriter, r *http.Request) {
	spl := strings.SplitN(r.URL.Path, ".", 2)
	hhash := spl[0]
	if len(hhash) != 65 {
		http.Error(w, "invalid /<sha256>[.ext] path", 400)
		return
	}
	hhash = hhash[1:]

	// check for an authorization tag, if any
	auth, err := readAuthorization(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// if there is one, we check if it has the extra requirements
	if auth != nil {
		if auth.Tags.GetFirst([]string{"t", "get"}) == nil {
			http.Error(w, "invalid Authorization event \"t\" tag", 403)
			return
		}

		if auth.Tags.GetFirst([]string{"x", hhash}) == nil &&
			auth.Tags.GetFirst([]string{"server", bs.ServiceURL}) == nil {
			http.Error(w, "invalid Authorization event \"x\" or \"server\" tag", 403)
			return
		}
	}

	for _, rg := range bs.RejectGet {
		reject, reason, code := rg(r.Context(), auth, hhash)
		if reject {
			http.Error(w, reason, code)
			return
		}
	}

	var ext string
	if len(spl) == 2 {
		ext = "." + spl[1]
	}

	for _, lb := range bs.LoadBlob {
		reader, _ := lb(r.Context(), hhash)
		if reader != nil {
			w.Header().Add("Content-Type", mime.TypeByExtension(ext))
			io.Copy(w, reader)
			return
		}
	}

	http.Error(w, "file not found", 404)
	return
}

func (bs BlossomServer) handleHasBlob(w http.ResponseWriter, r *http.Request) {
	spl := strings.SplitN(r.URL.Path, ".", 2)
	hhash := spl[0]
	if len(hhash) != 65 {
		http.Error(w, "invalid /<sha256>[.ext] path", 400)
		return
	}
	hhash = hhash[1:]

	bd, err := bs.Store.Get(r.Context(), hhash)
	if err != nil {
		http.Error(w, "failed to query: "+err.Error(), 500)
		return
	}

	if bd == nil {
		http.Error(w, "file not found", 404)
		return
	}

	return
}

func (bs BlossomServer) handleList(w http.ResponseWriter, r *http.Request) {
	// check for an authorization tag, if any
	auth, err := readAuthorization(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// if there is one, we check if it has the extra requirements
	if auth != nil {
		if auth.Tags.GetFirst([]string{"t", "list"}) == nil {
			http.Error(w, "invalid Authorization event \"t\" tag", 403)
			return
		}
	}

	pubkey := r.URL.Path[6:]

	for _, rl := range bs.RejectList {
		reject, reason, code := rl(r.Context(), auth, pubkey)
		if reject {
			http.Error(w, reason, code)
			return
		}
	}

	ch, err := bs.Store.List(r.Context(), pubkey)
	if err != nil {
		http.Error(w, "failed to query: "+err.Error(), 500)
		return
	}

	w.Write([]byte{'['})
	enc := json.NewEncoder(w)
	for bd := range ch {
		enc.Encode(bd)
	}
	w.Write([]byte{']'})
}

func (bs BlossomServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	auth, err := readAuthorization(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	if auth != nil {
		if auth.Tags.GetFirst([]string{"t", "delete"}) == nil {
			http.Error(w, "invalid Authorization event \"t\" tag", 403)
			return
		}
	}

	spl := strings.SplitN(r.URL.Path, ".", 2)
	hhash := spl[0]
	if len(hhash) != 65 {
		http.Error(w, "invalid /<sha256>[.ext] path", 400)
		return
	}
	hhash = hhash[1:]
	if auth.Tags.GetFirst([]string{"x", hhash}) == nil &&
		auth.Tags.GetFirst([]string{"server", bs.ServiceURL}) == nil {
		http.Error(w, "invalid Authorization event \"x\" or \"server\" tag", 403)
		return
	}

	for _, rd := range bs.RejectDelete {
		reject, reason, code := rd(r.Context(), auth, hhash)
		if reject {
			http.Error(w, reason, code)
			return
		}
	}

	for _, del := range bs.DeleteBlob {
		if err := del(r.Context(), hhash); err != nil {
			http.Error(w, "failed to delete blob: "+err.Error(), 500)
			return
		}
	}

	if err := bs.Store.Delete(r.Context(), hhash, auth.PubKey); err != nil {
		http.Error(w, "delete of blob entry failed: "+err.Error(), 500)
		return
	}
}

func (bs BlossomServer) handleMirror(w http.ResponseWriter, r *http.Request) {
}

func (bs BlossomServer) handleNegentropy(w http.ResponseWriter, r *http.Request) {
}