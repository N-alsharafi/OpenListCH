package handles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	stdpath "path"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/net"
	"github.com/OpenListTeam/OpenList/v4/internal/quota"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/gin-gonic/gin"
	"github.com/microcosm-cc/bluemonday"
	log "github.com/sirupsen/logrus"
	"github.com/yuin/goldmark"
)

func Down(c *gin.Context) {
	rawPath := c.Request.Context().Value(conf.PathKey).(string)
	filename := stdpath.Base(rawPath)
	storage, err := fs.GetStorage(rawPath, &fs.GetStoragesArgs{})
	if err != nil {
		common.ErrorPage(c, err, 500)
		return
	}
	if common.ShouldProxy(storage, filename) {
		Proxy(c)
		return
	} else {
		// Check quota before generating redirect URL for S3/direct storage
		if !checkDownloadQuota(c, conf.Conf.DownloadQuotaLimit) {
			return // Quota exceeded, error already sent
		}

		link, _, err := fs.Link(c.Request.Context(), rawPath, model.LinkArgs{
			IP:       c.ClientIP(),
			Header:   c.Request.Header,
			Type:     c.Query("type"),
			Redirect: true,
		})
		if err != nil {
			// Rollback quota on error
			rollbackDownloadQuota(c, conf.Conf.DownloadQuotaLimit)
			common.ErrorPage(c, err, 500)
			return
		}
		redirect(c, link)
	}
}

func Proxy(c *gin.Context) {
	rawPath := c.Request.Context().Value(conf.PathKey).(string)
	filename := stdpath.Base(rawPath)
	storage, err := fs.GetStorage(rawPath, &fs.GetStoragesArgs{})
	if err != nil {
		common.ErrorPage(c, err, 500)
		return
	}
	if canProxy(storage, filename) {
		if _, ok := c.GetQuery("d"); !ok {
			if url := common.GenerateDownProxyURL(storage.GetStorage(), rawPath); url != "" {
				c.Redirect(302, url)
				return
			}
		}
		link, file, err := fs.Link(c.Request.Context(), rawPath, model.LinkArgs{
			Header: c.Request.Header,
			Type:   c.Query("type"),
		})
		if err != nil {
			common.ErrorPage(c, err, 500)
			return
		}
		proxy(c, link, file, storage.GetStorage().ProxyRange)
	} else {
		common.ErrorPage(c, errors.New("proxy not allowed"), 403)
		return
	}
}

func redirect(c *gin.Context, link *model.Link) {
	defer link.Close()
	var err error
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Cache-Control", "max-age=0, no-cache, no-store, must-revalidate")
	if setting.GetBool(conf.ForwardDirectLinkParams) {
		query := c.Request.URL.Query()
		for _, v := range conf.SlicesMap[conf.IgnoreDirectLinkParams] {
			query.Del(v)
		}
		link.URL, err = utils.InjectQuery(link.URL, query)
		if err != nil {
			common.ErrorPage(c, err, 500)
			return
		}
	}
	c.Redirect(302, link.URL)
}

func proxy(c *gin.Context, link *model.Link, file model.Obj, proxyRange bool) {
	defer link.Close()
	var err error
	if link.URL != "" && setting.GetBool(conf.ForwardDirectLinkParams) {
		query := c.Request.URL.Query()
		for _, v := range conf.SlicesMap[conf.IgnoreDirectLinkParams] {
			query.Del(v)
		}
		link.URL, err = utils.InjectQuery(link.URL, query)
		if err != nil {
			common.ErrorPage(c, err, 500)
			return
		}
	}
	if proxyRange {
		link = common.ProxyRange(c, link, file.GetSize())
	}
	Writer := &common.WrittenResponseWriter{ResponseWriter: c.Writer}
	raw, _ := strconv.ParseBool(c.DefaultQuery("raw", "false"))
	if utils.Ext(file.GetName()) == "md" && setting.GetBool(conf.FilterReadMeScripts) && !raw {
		buf := bytes.NewBuffer(make([]byte, 0, file.GetSize()))
		w := &common.InterceptResponseWriter{ResponseWriter: Writer, Writer: buf}
		err = common.Proxy(w, c.Request, link, file)
		if err == nil && buf.Len() > 0 {
			if c.Writer.Status() < 200 || c.Writer.Status() > 300 {
				c.Writer.Write(buf.Bytes())
				return
			}

			var html bytes.Buffer
			if err = goldmark.Convert(buf.Bytes(), &html); err != nil {
				err = fmt.Errorf("markdown conversion failed: %w", err)
			} else {
				buf.Reset()
				err = bluemonday.UGCPolicy().SanitizeReaderToWriter(&html, buf)
				if err == nil {
					Writer.Header().Set("Content-Length", strconv.FormatInt(int64(buf.Len()), 10))
					Writer.Header().Set("Content-Type", "text/html; charset=utf-8")
					_, err = utils.CopyWithBuffer(Writer, buf)
				}
			}
		}
	} else {
		err = common.Proxy(Writer, c.Request, link, file)
	}
	if err == nil {
		return
	}
	if Writer.IsWritten() {
		log.Errorf("%s %s local proxy error: %+v", c.Request.Method, c.Request.URL.Path, err)
	} else {
		if statusCode, ok := errs.UnwrapOrSelf(err).(net.HttpStatusCodeError); ok {
			common.ErrorPage(c, err, int(statusCode), true)
		} else {
			common.ErrorPage(c, err, 500, true)
		}
	}
}

// TODO need optimize
// when can be proxy?
// 1. text file
// 2. config.MustProxy()
// 3. storage.WebProxy
// 4. proxy_types
// solution: text_file + shouldProxy()
func canProxy(storage driver.Driver, filename string) bool {
	if storage.Config().MustProxy() || storage.GetStorage().WebProxy || storage.GetStorage().WebdavProxyURL() {
		return true
	}
	if utils.SliceContains(conf.SlicesMap[conf.ProxyTypes], utils.Ext(filename)) {
		return true
	}
	if utils.SliceContains(conf.SlicesMap[conf.TextTypes], utils.Ext(filename)) {
		return true
	}
	return false
}

// checkDownloadQuota checks and reserves quota for direct downloads
// Returns true if allowed, false if blocked (and handles the response)
func checkDownloadQuota(c *gin.Context, limit int) bool {
	// Skip HEAD requests
	if c.Request.Method == "HEAD" {
		return true
	}

	// Skip if user is authenticated
	if user, ok := c.Value(conf.UserKey).(*model.User); ok && user != nil {
		return true
	}

	// Get connection identifier
	ip := c.ClientIP()
	userAgent := c.Request.UserAgent()
	acceptLang := c.Request.Header.Get("Accept-Language")
	connID := quota.GetConnectionID(ip, userAgent, acceptLang)

	// Count how many downloads this request represents
	fileCount := 1

	// For range requests, count each range separately
	if rangeHeader := c.Request.Header.Get("Range"); rangeHeader != "" {
		if strings.HasPrefix(rangeHeader, "bytes=") {
			ranges := strings.Split(rangeHeader[6:], ",")
			fileCount = len(ranges)
		}
	}

	// Count files in folder if applicable (3 levels deep)
	if pathObj := c.Request.Context().Value(conf.PathKey); pathObj != nil {
		path := pathObj.(string)
		count, err := countFilesInPath(c.Request.Context(), path)
		if err == nil && count > 0 {
			fileCount = count
		}
	}

	// Check and reserve quota atomically
	allowed, _, err := quota.CheckAndReserve(connID, fileCount, limit)
	if err != nil {
		c.AbortWithStatusJSON(500, gin.H{
			"code":    500,
			"message": "Error checking download quota",
		})
		return false
	}

	if !allowed {
		// Check if request is from a browser (expects HTML)
		accept := c.Request.Header.Get("Accept")
		isBrowser := strings.Contains(accept, "text/html") && !strings.Contains(accept, "application/json")

		if isBrowser {
			// Return HTML error page for browsers
			c.Header("Content-Type", "text/html; charset=utf-8")
			c.AbortWithStatus(429)
			c.Writer.WriteString(generateQuotaErrorHTML(limit))
		} else {
			// Return empty body for API clients
			c.AbortWithStatus(429)
		}
		return false
	}

	return true
}

// rollbackDownloadQuota removes reserved quota when download fails
func rollbackDownloadQuota(c *gin.Context, limit int) {
	// Skip for authenticated users
	if user, ok := c.Value(conf.UserKey).(*model.User); ok && user != nil {
		return
	}

	ip := c.ClientIP()
	userAgent := c.Request.UserAgent()
	acceptLang := c.Request.Header.Get("Accept-Language")
	connID := quota.GetConnectionID(ip, userAgent, acceptLang)

	fileCount := 1

	// Count range requests
	if rangeHeader := c.Request.Header.Get("Range"); rangeHeader != "" {
		if strings.HasPrefix(rangeHeader, "bytes=") {
			ranges := strings.Split(rangeHeader[6:], ",")
			fileCount = len(ranges)
		}
	}

	// Count files in folder
	if pathObj := c.Request.Context().Value(conf.PathKey); pathObj != nil {
		path := pathObj.(string)
		count, err := countFilesInPath(c.Request.Context(), path)
		if err == nil && count > 0 {
			fileCount = count
		}
	}

	quota.Decrement(connID, fileCount)
}

// countFilesInPath recursively counts files in a path up to 3 levels deep
func countFilesInPath(ctx context.Context, path string) (int, error) {
	objs, err := fs.List(ctx, path, &fs.ListArgs{})
	if err != nil {
		return 0, err
	}

	return countFilesRecursive(ctx, objs, 0, 3), nil
}

// countFilesRecursive counts files recursively up to maxDepth levels
func countFilesRecursive(ctx context.Context, objs []model.Obj, currentDepth, maxDepth int) int {
	if currentDepth >= maxDepth {
		return 0
	}

	count := 0
	for _, obj := range objs {
		if obj.IsDir() {
			// It's a folder, recurse into it
			if currentDepth < maxDepth-1 {
				subObjs, err := fs.List(ctx, obj.GetPath(), &fs.ListArgs{})
				if err == nil {
					count += countFilesRecursive(ctx, subObjs, currentDepth+1, maxDepth)
				}
			}
		} else {
			// It's a file, count it
			count++
		}
	}

	return count
}

// generateQuotaErrorHTML creates a user-friendly HTML error page
func generateQuotaErrorHTML(limit int) string {
	return `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Download Limit Reached</title>
    <style>
        * {
            margin: 0;
            padding: 0;
            box-sizing: border-box;
        }
        
        body {
            background: linear-gradient(135deg, #1a1a2e 0%, #16213e 100%);
            color: #eaeaea;
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif;
            min-height: 100vh;
            display: flex;
            align-items: center;
            justify-content: center;
            padding: 20px;
        }
        
        .container {
            text-align: center;
            max-width: 500px;
            padding: 40px;
            background: rgba(255, 255, 255, 0.05);
            border-radius: 16px;
            border: 1px solid rgba(255, 255, 255, 0.1);
            backdrop-filter: blur(10px);
        }
        
        .icon {
            font-size: 64px;
            margin-bottom: 20px;
        }
        
        h1 {
            font-size: 28px;
            margin-bottom: 16px;
            color: #fff;
        }
        
        .message {
            font-size: 16px;
            line-height: 1.6;
            margin-bottom: 24px;
            color: #b8b8b8;
        }
        
        .limit-info {
            background: rgba(231, 76, 60, 0.2);
            border: 1px solid rgba(231, 76, 60, 0.3);
            border-radius: 8px;
            padding: 16px;
            margin-bottom: 24px;
        }
        
        .limit-info strong {
            color: #e74c3c;
            font-size: 18px;
        }
        
        .button {
            display: inline-block;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            text-decoration: none;
            padding: 14px 32px;
            border-radius: 8px;
            font-weight: 600;
            font-size: 16px;
            transition: transform 0.2s, box-shadow 0.2s;
            border: none;
            cursor: pointer;
        }
        
        .button:hover {
            transform: translateY(-2px);
            box-shadow: 0 8px 20px rgba(102, 126, 234, 0.4);
        }
        
        .button-secondary {
            display: inline-block;
            color: #a0a0a0;
            text-decoration: none;
            padding: 14px 32px;
            font-size: 14px;
            margin-top: 12px;
        }
        
        .button-secondary:hover {
            color: #fff;
        }
        
        @media (max-width: 480px) {
            .container {
                padding: 30px 20px;
            }
            
            h1 {
                font-size: 24px;
            }
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="icon">⏸️</div>
        <h1>Download Limit Reached</h1>
        <div class="limit-info">
            <strong>Limit: ` + strconv.Itoa(limit) + ` files per 24 hours</strong>
        </div>
        <p class="message">
            You've reached the download limit for anonymous users. To continue downloading files, please log in to your account.
        </p>
        <a href="/login" class="button">Log In to Continue</a>
        <br>
        <a href="javascript:history.back()" class="button-secondary">← Go Back</a>
    </div>
</body>
</html>`
}
