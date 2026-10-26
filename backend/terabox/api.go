package terabox

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	libPath "path"

	"github.com/rclone/rclone/backend/terabox/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/rest"
)

// Chunk of data representation
type Chunk struct {
	Data   []byte
	Readed int
	Number int
	MD5    string
}

// Reset state between use
func (t *Chunk) Reset() {
	t.Data = t.Data[:cap(t.Data)]
	t.Readed = 0
	t.Number = 0
	t.MD5 = ""
}

// Sync - syncing size of buffer with readed size; calculate MD5 hash
func (t *Chunk) Sync() {
	t.Data = t.Data[:t.Readed]
	t.MD5 = fmt.Sprintf("%x", md5.Sum(t.Data))
}

var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
	509, // Bandwidth Limit Exceeded
}

func (f *Fs) apiExec(ctx context.Context, opts *rest.Opts, res any) error {
	if opts == nil {
		return fmt.Errorf("empty request")
	}
	opts.IgnoreStatus = true

	retry := 0

retry:
	// TeraBox sometimes accepts a request (especially /api/create) and never
	// responds - with no per-request timeout the transfer hangs forever.
	// Cancel metadata requests 60s after start and retry. Chunk uploads
	// (/superfile2) stream data and must not be killed by a 60s timeout.
	callCtx := ctx
	if !strings.Contains(opts.RootURL, "/superfile2") {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	if !f.notFirstRun {
		f.client.SetRoot(f.baseURL)
		f.client.SetHeader("Accept", "application/json, text/plain, */*")
		if f.accessToken == "" {
			f.client.SetHeader("Referer", baseURL)
			f.client.SetHeader("X-Requested-With", "XMLHttpRequest")
			f.client.SetHeader("Cookie", f.opt.Cookie)
		}
		f.notFirstRun = true
	}

	if opts.Parameters != nil {
		if f.accessToken == "" {
			opts.Parameters.Set("app_id", "250528")
			opts.Parameters.Set("channel", "dubox")
			opts.Parameters.Set("clienttype", "0")
			opts.Parameters.Set("web", "1")
			if f.jsToken != "" {
				opts.Parameters.Set("jsToken", f.jsToken)
			}
		} else {
			opts.Parameters.Set("access_tokens", f.accessToken)
		}
	}

	if opts.Method == http.MethodPost && opts.MultipartParams != nil {
		// Build the multipart body on every attempt. Previously this only ran
		// on the first attempt, so a retry sent no body at all and the server
		// answered errno 2 "Required parameters are missing".
		var overhead int64
		var err error
		opts.Body, opts.ContentType, overhead, err = rest.MultipartUpload(ctx, opts.Body, opts.MultipartParams, opts.MultipartContentName, opts.MultipartFileName)
		if err != nil {
			return err
		}
		if opts.ContentLength != nil {
			*opts.ContentLength += overhead
		}
	}

	var reqBody *bytes.Buffer
	if f.opt.DebugLevel >= 4 && opts.Body != nil && !strings.Contains(opts.RootURL, "/superfile2") {
		reqBody = bytes.NewBuffer(make([]byte, 0))
		opts.Body = io.TeeReader(opts.Body, reqBody)
	}

	resp, err := f.client.Call(callCtx, opts)
	if err != nil {
		// retry timeouts and temporary network errors a few times -
		// TeraBox sometimes just stops responding mid-sync
		if retry < 3 && (errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "Client.Timeout") || strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "connection reset")) {
			retry++
			debug(f.opt, 1, "request error %q, retry %d", err, retry)
			time.Sleep(time.Duration(retry) * time.Second)
			goto retry
		}
		return err
	}

	debug(f.opt, 3, "Request: %+v", resp.Request)
	if reqBody != nil {
		debug(f.opt, 4, "Request body: %s", reqBody.String())
	}

	debug(f.opt, 2, "Response: %+v", resp)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	debug(f.opt, 2, "Response body: %s", body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err = fmt.Errorf("http error %d: %v", resp.StatusCode, resp.Status)
		debug(f.opt, 1, "Error: %s", err)
		if IsInSlice(resp.StatusCode, retryErrorCodes) {
			retry++
			if retry > 2 {
				return err
			}

			time.Sleep(time.Duration(retry) * time.Second)
			goto retry
		}
		return err
	}

	if err := json.Unmarshal(body, res); err != nil {
		return err
	}

	var jsTokenRequested bool
	if _, skip := res.(*api.ResponseUploadedChunk); !skip {
		if err, ok := res.(api.ErrorInterface); ok {
			if api.ErrIsNum(err, 4000023, 450016) && !jsTokenRequested {
				jsTokenRequested = true
				if err := f.apiJsToken(ctx); err != nil {
					return err
				}

				retry++
				goto retry
			} else if prefix, ok := resp.Header["Url-Domain-Prefix"]; ok && len(prefix) > 0 && api.ErrIsNum(err, -6) { // for some accounts base url can be different, then for others, update it
				newBaseURL := "https://" + prefix[0] + ".terabox.com"
				f.client.SetRoot(newBaseURL)
				debug(f.opt, 1, "Base URL changed from %s to %s", f.baseURL, newBaseURL)

				retry++
				goto retry
			}

			// /api/filemanager reports the useful per-item error (e.g. 31034
			// rate limit) in info[], while the top level errno is a generic 12.
			// Prefer the per-item error so callers can retry it.
			if operational, ok := res.(*api.ResponseOperational); ok {
				for _, item := range operational.Info {
					if itemErr := item.Err(); itemErr != nil {
						return itemErr
					}
				}
			}

			// surface the API error - callers rely on it to distinguish
			// permanent failures (invalid name, quota, ...) from success.
			// Err() returns nil when errno == 0.
			return err.Err()
		} else {
			return fmt.Errorf("response have no api error interface")
		}
	}

	return nil
}

func (f *Fs) apiJsToken(ctx context.Context) error {
	// fetch /main like the web browser does - the jsToken is embedded in that page
	res, err := f.client.Call(ctx, &rest.Opts{Method: http.MethodGet, Path: "/main"})
	if err != nil {
		return err
	}

	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	jsToken := getStrBetween(string(body), "`function%20fn%28a%29%7Bwindow.jsToken%20%3D%20a%7D%3Bfn%28%22", "%22%29`")
	if jsToken == "" {
		debug(f.opt, 3, "jsToken not found, body: %s", string(body))
		return fmt.Errorf("jsToken not found")
	}

	f.jsToken = jsToken
	return nil
}

func (f *Fs) apiCheckLogin(ctx context.Context) error {
	var res api.ResponseDefault
	err := f.apiExec(ctx, NewRequest(http.MethodGet, "/api/check/login"), &res)
	if err != nil {
		return err
	}

	return nil
}

func (f *Fs) apiCheckPremium(ctx context.Context) error {
	var res api.ResponseUser
	opt := NewRequest(http.MethodGet, "/rest/2.0/membership/proxy/user")
	opt.Parameters.Set("method", "query")
	opt.Parameters.Set("membership_version", "1.0")

	err := f.apiExec(ctx, opt, &res)
	if err != nil {
		return err
	}

	// Premium type: 0: regular user; 1: regular Premium; 2: super Premium
	f.isPremium = res.Data.MemberInfo.IsVIP > 0
	return nil
}

func (f *Fs) apiList(ctx context.Context, dir string) ([]*api.Item, error) {
	if len(dir) == 0 || dir[0] != '/' {
		dir = "/" + dir
	}

	page := 1
	limit := 100
	opt := NewRequest(http.MethodGet, "/api/list")
	opt.Parameters.Set("dir", dir)
	// opt.Parameters.Set("web", "1") // If 1 is passed, the thumbnail field thumbs will be returned.
	// opt.Parameters.Set("order", ...) // Sorting field: time (modification time), name (file name), size (size; note that directories do not have a size)
	// if true {
	// 	opt.Parameters.Set("desc", "1") // 1: descending order; 0: ascending order
	// }

	list := make([]*api.Item, 0)
	for {
		opt.Parameters.Set("page", strconv.Itoa(page))
		opt.Parameters.Set("num", strconv.Itoa(limit))

		var res api.ResponseList
		err := f.apiExec(ctx, opt, &res)
		if err != nil {
			return nil, err
		}

		list = append(list, res.List...)

		if len(res.List) == 0 || len(res.List) < limit {
			break
		}

		page++
	}

	return list, nil
}

// files info, can return info about a few files, but we're use it for only one file
func (f *Fs) apiItemInfo(ctx context.Context, path string, downloadLink bool) (*api.Item, error) {
	opt := NewRequest(http.MethodGet, "/api/filemetas")
	opt.Parameters.Set("target", fmt.Sprintf(`["%s"]`, path))
	if downloadLink {
		opt.Parameters.Set("dlink", "1")
	} else {
		opt.Parameters.Set("dlink", "0")
	}

	var res api.ResponseItemInfo
	err := f.apiExec(ctx, opt, &res)
	// firstly we will check error for a file, and only then one for full operation {"errno":12,"info":[{"errno":-9}],"request_id":8798843383335989660}
	if len(res.List) > 0 {
		if res.List[0].Err() != nil {
			return nil, res.List[0].Err()
		}

		return &res.List[0].Item, nil
	}

	if err != nil {
		return nil, err
	}

	return nil, fs.ErrorObjectNotFound
}

func (f *Fs) apiMkDir(ctx context.Context, path string) error {
	if err := checkRemoteName(path); err != nil {
		return err
	}
	opt := NewRequest(http.MethodPost, "/api/create")
	opt.MultipartParams = url.Values{}
	opt.MultipartParams.Set("path", path)
	opt.MultipartParams.Set("isdir", "1")
	opt.MultipartParams.Set("rtype", "0") // The file naming policy. The default value is 1. 0: Do not rename. If a file with the same name exists in the cloud, this call will fail and return a conflict; 1: Rename if there is any path conflict; 2: Rename only if there is a path conflict and the block_list is different; 3: Overwrite

	var res api.ResponseDefault
	err := f.apiExec(ctx, opt, &res)
	return err
}

// operation - copy (file copy), move (file movement), rename (file renaming), and delete (file deletion)
// opera=copy: filelist: [{"path":"/hello/test.mp4","dest":"","newname":"test.mp4"}]
// opera=move: filelist: [{"path":"/test.mp4","dest":"/test_dir","newname":"test.mp4"}]
// opera=rename: filelist：[{"path":"/hello/test.mp4","newname":"test_one.mp4"}]
// opera=delete: filelist: ["/test.mp4"]
func (f *Fs) apiOperation(ctx context.Context, operation string, items []api.OperationalItem) error {
	f.operationMu.Lock()
	defer f.operationMu.Unlock()
	if wait := time.Second - time.Since(f.lastOperation); wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	defer func() { f.lastOperation = time.Now() }()

	opt := NewRequest(http.MethodPost, "/api/filemanager")
	opt.Parameters.Set("opera", operation)
	// async=0 is what the web client and alist use for delete/move/copy.
	// async=1 makes the server report per-item errno 12 / 31034 for deletes
	// even when the item exists.
	opt.Parameters.Set("async", "0")
	opt.Parameters.Set("onnest", "fail")

	// get JS token
	if f.jsToken == "" {
		if err := f.apiJsToken(ctx); err != nil {
			return err
		}
	}

	var list any
	if operation == "delete" {
		list = make([]string, len(items))
		for idx, item := range items {
			list.([]string)[idx] = item.Path
		}
	} else {
		list = items
	}

	mItems, err := json.Marshal(list)
	if err != nil {
		return err
	}

	body := fmt.Sprintf("async=0&filelist=%s&ondup=newcopy", strings.ReplaceAll(url.QueryEscape(string(mItems)), "+", "%20"))

	var res api.ResponseOperational
	// 31034 means the server is throttling this operation. Deletes from sync
	// arrive in bursts, so back off and retry instead of failing the file.
	for attempt := 0; ; attempt++ {
		// The request body is consumed by each call, so recreate it per attempt.
		opt.Body = bytes.NewBufferString(body)
		err := f.apiExec(ctx, opt, &res)
		if err == nil {
			for _, oi := range res.Info {
				if oi.Err() != nil {
					err = oi.Err()
					break
				}
			}
		}
		if err == nil {
			break
		}
		// 31034: throttled. 111: a previous async file operation is still
		// running. Both are transient, so back off and retry.
		if !api.ErrIsNum(err, 31034, 111) || attempt >= 6 {
			return err
		}
		delay := time.Duration(1<<attempt) * time.Second
		debug(f.opt, 1, "%s temporarily unavailable (%v), retry %d in %v", operation, err, attempt+1, delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	if operation == "delete" && f.opt.DeletePermanently {
		if err := f.apiCleanRecycleBin(ctx); err != nil {
			return err
		}
	}

	return nil
}

// Download file
func (f *Fs) apiDownloadLink(ctx context.Context, fileID uint64) (*api.ResponseDownload, error) {
	signKeys, err := f.apiSignPrepare(ctx)
	if err != nil {
		return nil, err
	}

	opt := NewRequest(http.MethodGet, "/api/download")
	opt.Parameters.Set("type", "dlink")
	opt.Parameters.Set("vip", "2")
	opt.Parameters.Set("sign", sign(signKeys[0], signKeys[1]))
	opt.Parameters.Set("timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	opt.Parameters.Set("need_speed", "1")
	opt.Parameters.Set("fidlist", fmt.Sprintf("[%d]", fileID))

	var res api.ResponseDownload
	if err := f.apiExec(ctx, opt, &res); err != nil {
		return nil, err
	}

	return &res, nil
}

func (f *Fs) apiSignPrepare(ctx context.Context) ([]string, error) {
	opt := NewRequest(http.MethodGet, "/api/home/info")

	var res api.ResponseHomeInfo
	if err := f.apiExec(ctx, opt, &res); err != nil {
		return nil, err
	}

	return []string{res.Data.Sign3, res.Data.Sign1}, nil
}

// Delete files from Recycle Bin
func (f *Fs) apiCleanRecycleBin(ctx context.Context) error {
	opt := NewRequest(http.MethodPost, "/api/recycle/clear")
	opt.Parameters.Set("async", "0") // The default value is 0; 0: synchronous; 1: adaptive; 2: asynchronous. The difference lies in whether to care about the success of the request, and the returned structure differs. Different structures are returned based on the request parameters; see the return examples for details.)

	var res api.ResponseDefault
	if err := f.apiExec(ctx, opt, &res); err != nil {
		return err
	}

	return nil
}

// Quota limits for storage
func (f *Fs) apiQuotaInfo(ctx context.Context) (*api.ResponseQuota, error) {
	opt := NewRequest(http.MethodGet, "/api/quota")
	opt.Parameters.Set("checkexpire", "1")
	opt.Parameters.Set("checkfree", "1")

	var res api.ResponseQuota
	if err := f.apiExec(ctx, opt, &res); err != nil {
		return nil, err
	}

	return &res, nil
}

// Upload file
func (f *Fs) apiFileUpload(ctx context.Context, path string, size int64, modTime time.Time, in io.Reader, options []fs.OpenOption, overwriteMode uint8) error {
	if err := checkRemoteName(path); err != nil {
		return err
	}
	f.isPremiumMX.Do(func() {
		_ = f.apiCheckPremium(ctx)
	})

	// freeFileLimitSize - 4GB; premiumFileLimitSize - 128GB
	if (!f.isPremium && size > int64(4*fs.Gibi)) || (f.isPremium && size > int64(128*fs.Gibi)) {
		return api.Num2Err(58)
	}

	// get host for upload
	var err error
	f.uploadHostMX.Do(func() {
		err = f.apiFileLocateUpload(ctx)
	})
	if err != nil {
		f.uploadHostMX = sync.Once{}
		return err
	}

	// get JS token
	if f.jsToken == "" {
		if err := f.apiJsToken(ctx); err != nil {
			return err
		}
	}

	// hash the file once - needed for rapid upload and for precreate/create
	rapidInfo, err := rapidUploadInfo(in, size, getChunkSize(size, f.isPremium))
	if err != nil {
		return fmt.Errorf("failed to hash file for upload: %w", err)
	}

	// try rapid upload (server side dedup) - the official client skips it
	// for files smaller than 256KiB
	if size >= 256*1024 {
		res, err := f.apiFileRapidUpload(ctx, path, size, modTime, rapidInfo.sliceMD5, rapidInfo.contentMD5, rapidInfo.contentCrc32, overwriteMode)
		if err == nil && res.Info.Size == size {
			// TeraBox indexes new files asynchronously - the file may not be
			// visible to filemetas for a short while, so wait for it here to
			// avoid a spurious "object not found" right after Put.
			fs.Infof(nil, "rapid upload of %q succeeded (fs_id %d), no data transfer needed", path, res.Info.ID)
			for wait := 0; wait < 40; wait++ {
				if _, err := f.apiItemInfo(ctx, path, false); err == nil {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			return nil
		}
		debug(f.opt, 1, "rapid upload of %q not possible (err=%v), falling back to chunked upload", path, err)
	}

	// precreate file
	resPreCreate, err := f.apiFilePrecreate(ctx, path, size, modTime, rapidInfo.blockList)
	if err != nil {
		return err
	}

	if resPreCreate.Type == 2 {
		return api.Num2Err(-8)
	}

	// upload chunks - blockList already computed during the rapid upload attempt
	chunksUploaded := map[int]string{}
	mx := sync.Mutex{}
	threads := sync.WaitGroup{}
	threadsLimitter := make(chan struct{}, f.opt.UploadThreads)
	threadsErrs := []string{}
	for chunkIndex, chunkMD5 := range rapidInfo.blockList {
		// check context
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// use the data saved during hashing directly
		chunk := &Chunk{
			Data:   rapidInfo.blockData[chunkIndex],
			Readed: len(rapidInfo.blockData[chunkIndex]),
			Number: chunkIndex,
			MD5:    chunkMD5,
		}

		chunksUploaded[chunk.Number] = chunk.MD5

		// wait available slot for upload
		threadsLimitter <- struct{}{}

		// if upload broken, finish it
		if len(threadsErrs) > 0 {
			return fmt.Errorf("chunks upload error(s): %s", strings.Join(threadsErrs, "; "))
		}

		// upload start
		threads.Add(1)
		go func(chunk *Chunk) {
			defer func() { // release upload slot
				<-threadsLimitter
				threads.Done()
			}()

			attemptChunkUpload := 0
			for attemptChunkUpload < 4 {
				// check context
				if ctx.Err() != nil {
					mx.Lock()
					threadsErrs = append(threadsErrs, ctx.Err().Error())
					mx.Unlock()
					return
				}

				// upload chunk
				resUpload, err := f.apiFileUploadChunk(ctx, path, resPreCreate.UploadID, chunk.Number, int64(chunk.Readed), chunk.Data, options)
				if err != nil {
					attemptChunkUpload++
					debug(f.opt, 1, "upload chunk (%d) error: %s | attempt: %d", chunk.Number, err, attemptChunkUpload)
					if attemptChunkUpload > 3 {
						mx.Lock()
						threadsErrs = append(threadsErrs, err.Error())
						mx.Unlock()
						return
					}

					continue
				}

				debug(f.opt, 1, "chunk %d md5 %s chunk size %d", chunk.Number, chunk.MD5, chunk.Readed)

				if chunk.MD5 != resUpload.MD5 {
					attemptChunkUpload++
					debug(f.opt, 1, "uploaded chunk (%d) have wrong md5: our: %s | uploaded: %s | attempt: %d", chunk.Number, chunk.MD5, resUpload.MD5, attemptChunkUpload)
					if attemptChunkUpload > 3 {
						mx.Lock()
						threadsErrs = append(threadsErrs, fmt.Sprintf("can't upload chunk %d with three attempts, server hash of chunk %s is different, than ours %s", chunk.Number, resUpload.MD5, chunk.MD5))
						mx.Unlock()
						return
					}

					continue
				} else {
					break
				}
			}
		}(chunk)
	}
	threads.Wait()
	close(threadsLimitter)
	if len(threadsErrs) > 0 {
		return fmt.Errorf("chunks upload error(s): %s", strings.Join(threadsErrs, "; "))
	}

	chunksUploadedList := make([]string, len(chunksUploaded))
	for k, v := range chunksUploaded {
		chunksUploadedList[k] = v
	}

	attemptFileCreate := 0

retryFileCreate:
	// check context
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// create file
	created, err := f.apiFileCreate(ctx, path, resPreCreate.UploadID, size, modTime, chunksUploadedList, overwriteMode)
	if err != nil {
		if api.ErrIsNum(err, -8) {
			if item, infoErr := f.apiItemInfo(ctx, path, false); infoErr == nil {
				// A leftover directory with the same name (e.g. from a
				// previous failed upload) blocks the file - remove it.
				if item.Isdir > 0 {
					fs.Logf(nil, "%q exists as a directory, removing it to upload the file", path)
					if delErr := f.apiOperation(ctx, "delete", []api.OperationalItem{{Path: path}}); delErr != nil {
						return delErr
					}
					goto retryFileCreate
				}
				// sync state can be stale - if the file already has the right
				// size the upload is effectively done
				if item.Size == size {
					fs.Infof(nil, "%q already exists with the same size, skipping", path)
					return nil
				}
			}
		}

		// API errors (e.g. errno -7 "param path file name is invalid") are
		// permanent - retrying them just spins forever. Only retry transport
		// level failures.
		if _, ok := err.(api.ErrorInterface); ok {
			return err
		}
		if attemptFileCreate < 3 {
			attemptFileCreate++
			debug(f.opt, 1, "create file error %q, attempt %d", err, attemptFileCreate)
			goto retryFileCreate
		}
		return err
	}
	attemptFileCreate = 0

	// /api/create's md5 field is not always the file's real MD5: for some
	// files the server returns an encoded value (e.g. a9ef9f469ua08... for
	// 170b40ada8cc...). /api/filemetas then returns the correct hash. Treat
	// a non-hex create hash as "unverified" instead of failing an upload
	// that already succeeded; only a valid, differing hash is an error.
	finalMD5 := decodeMD5(created.MD5)
	controlMD5 := ""
	if len(chunksUploadedList) == 1 {
		controlMD5 = chunksUploadedList[0]
	} else {
		jsonBytes, err := json.Marshal(chunksUploadedList)
		if err != nil {
			return err
		}
		controlMD5 = fmt.Sprintf("%x", md5.Sum(jsonBytes))
	}

	if isHexMD5(finalMD5) && controlMD5 != finalMD5 {
		debug(f.opt, 1, "controlMD5 %s not equal server file md5 %s, attempt %d", controlMD5, finalMD5, attemptFileCreate)
		if attemptFileCreate < 3 {
			attemptFileCreate++
			goto retryFileCreate
		}

		return fmt.Errorf("can't create file control MD5 is different than ours")
	}

	return nil
}

func (f *Fs) apiFileLocateUpload(ctx context.Context) error {
	// NOTE: keep the request without upload_version=2.0 - the v2 response
	// drops the "host" field in favour of a "servers" list (observed live)
	opt := NewRequest(http.MethodGet, "/rest/2.0/pcs/file?method=locateupload")
	opt.Parameters = nil

	var res api.ResponseFileLocateUpload
	if err := f.apiExec(ctx, opt, &res); err != nil {
		return err
	}
	debug(f.opt, 1, "locateupload response: %+v", res)

	f.uploadHost = res.Host

	return nil
}

func (f *Fs) apiFilePrecreate(ctx context.Context, path string, size int64, modTime time.Time, blockList []string) (*api.ResponsePrecreate, error) {
	opt := NewRequest(http.MethodPost, "/api/precreate")

	opt.MultipartParams = url.Values{}
	opt.MultipartParams.Set("path", path)
	opt.MultipartParams.Set("autoinit", "1")
	opt.MultipartParams.Set("local_mtime", fmt.Sprintf("%d", modTime.Unix()))
	opt.MultipartParams.Set("file_limit_switch_v34", "true")
	opt.MultipartParams.Set("size", fmt.Sprintf("%d", size))

	dirPath, _ := libPath.Split(path)
	opt.MultipartParams.Set("target_path", dirPath)

	// the official client sends the real md5 hash list of the chunks here;
	// a placeholder list makes the server reject the file at the create step
	blockListStr, err := json.Marshal(blockList)
	if err != nil {
		return nil, err
	}
	opt.MultipartParams.Set("block_list", string(blockListStr))

	var res api.ResponsePrecreate
	err = f.apiExec(ctx, opt, &res)
	if err != nil {
		return nil, err
	}

	return &res, nil
}

// apiFileRapidUpload tries to instant-upload a file whose md5 the server
// already knows (dedup), skipping data transfer entirely.
func (f *Fs) apiFileRapidUpload(ctx context.Context, path string, size int64, modTime time.Time, md5Slice, contentMD5, contentCrc32 string, overwriteMode uint8) (*api.ResponseRapidUpload, error) {
	opt := NewRequest(http.MethodPost, "/api/rapidupload")
	opt.Parameters.Set("rtype", fmt.Sprintf("%d", overwriteMode))
	if overwriteMode > 3 {
		opt.Parameters.Set("rtype", "1")
	}

	// params mirror the official web client (see app.asar index.js):
	// path, content-md5, slice-md5, content-length, target_path
	opt.MultipartParams = url.Values{}
	opt.MultipartParams.Set("path", path)
	opt.MultipartParams.Set("content-length", fmt.Sprintf("%d", size))
	opt.MultipartParams.Set("local_mtime", fmt.Sprintf("%d", modTime.Unix()))
	opt.MultipartParams.Set("slice-md5", md5Slice)
	opt.MultipartParams.Set("content-md5", contentMD5)
	if contentCrc32 != "" {
		opt.MultipartParams.Set("content-crc32", contentCrc32)
	}

	dirPath, _ := libPath.Split(path)
	opt.MultipartParams.Set("target_path", dirPath)

	var res api.ResponseRapidUpload
	if err := f.apiExec(ctx, opt, &res); err != nil {
		return nil, err
	}

	return &res, nil
}

func (f *Fs) apiFileUploadChunk(ctx context.Context, path, uploadID string, chunkNumber int, size int64, data []byte, options []fs.OpenOption) (*api.ResponseUploadedChunk, error) {
	opt := NewRequest(http.MethodPost, fmt.Sprintf("https://%s/rest/2.0/pcs/superfile2", f.uploadHost))
	opt.Parameters.Set("method", "upload")
	opt.Parameters.Set("path", path)
	opt.Parameters.Set("uploadid", uploadID)
	opt.Parameters.Set("partseq", fmt.Sprintf("%d", chunkNumber))
	opt.Parameters.Set("uploadsign", "0")
	opt.Options = options

	formReader, contentType, overhead, err := rest.MultipartUpload(ctx, bytes.NewReader(data), opt.MultipartParams, "file", "blob")
	if err != nil {
		return nil, fmt.Errorf("failed to make multipart upload for file: %w", err)
	}
	contentLength := overhead + size
	opt.ContentLength = &contentLength
	opt.ContentType = contentType
	opt.Body = formReader

	var res api.ResponseUploadedChunk
	if err := f.apiExec(ctx, opt, &res); err != nil {
		return nil, err
	}

	return &res, nil
}

func (f *Fs) apiFileCreate(ctx context.Context, path, uploadID string, size int64, modTime time.Time, blockList []string, overwriteMode uint8) (*api.ResponseCreate, error) {
	opt := NewRequest(http.MethodPost, "/api/create")
	opt.Parameters.Set("isdir", "0")

	// The file naming policy. The default value is 1. 0: Do not rename. If a file with the same name exists in the cloud, this call will fail and return a conflict; 1: Rename if there is any path conflict; 2: Rename only if there is a path conflict and the block_list is different; 3: Overwrite
	opt.Parameters.Set("rtype", fmt.Sprintf("%d", overwriteMode))
	if overwriteMode > 3 {
		opt.Parameters.Set("rtype", "1")
	}

	opt.MultipartParams = url.Values{}
	opt.MultipartParams.Set("path", path)
	// opt.MultipartParams.Set("isdir", "0") // for dir create this param shold be in body, for upload in URL
	// opt.MultipartParams.Set("rtype", "0") // for dir create this param shold be in body, for upload in URL
	opt.MultipartParams.Set("local_mtime", fmt.Sprintf("%d", modTime.Unix()))
	opt.MultipartParams.Set("uploadid", uploadID)
	opt.MultipartParams.Set("size", fmt.Sprintf("%d", size))

	dirPath, _ := libPath.Split(path)
	opt.MultipartParams.Set("target_path", dirPath)

	blockListStr, err := json.Marshal(blockList)
	if err != nil {
		return nil, err
	}
	opt.MultipartParams.Set("block_list", string(blockListStr))

	debug(f.opt, 3, "%+v", opt.MultipartParams)

	var res api.ResponseCreate
	if err := f.apiExec(ctx, opt, &res); err != nil {
		return nil, err
	}

	return &res, nil
}
