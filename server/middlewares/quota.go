package middlewares

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/quota"
	"github.com/gin-gonic/gin"
)

// DownloadQuotaLimiter creates a middleware that limits anonymous downloads
func DownloadQuotaLimiter(limit int) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip HEAD requests
		if c.Request.Method == "HEAD" {
			c.Next()
			return
		}

		// Skip if user is authenticated
		if user, ok := c.Value(conf.UserKey).(*model.User); ok && user != nil {
			c.Next()
			return
		}

		// Get connection identifier
		ip := c.ClientIP()
		userAgent := c.Request.UserAgent()
		acceptLang := c.Request.Header.Get("Accept-Language")
		connID := quota.GetConnectionID(ip, userAgent, acceptLang)

		// Count how many downloads this request represents
		// For range requests, count each range separately
		rangeCount := 1
		if rangeHeader := c.Request.Header.Get("Range"); rangeHeader != "" {
			// HTTP Range format: bytes=start-end,start-end,...
			// Count commas to determine number of ranges
			if strings.HasPrefix(rangeHeader, "bytes=") {
				ranges := strings.Split(rangeHeader[6:], ",")
				rangeCount = len(ranges)
			}
		}

		// Check remaining quota
		remaining, err := quota.CheckRemaining(connID, limit)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    500,
				"message": "Error checking download quota",
			})
			return
		}

		if remaining < rangeCount {
			// Check if request is from a browser (expects HTML)
			accept := c.Request.Header.Get("Accept")
			isBrowser := strings.Contains(accept, "text/html") && !strings.Contains(accept, "application/json")
			
			if isBrowser {
				// Return HTML error page for browsers
				c.Header("Content-Type", "text/html; charset=utf-8")
				c.AbortWithStatus(http.StatusTooManyRequests)
				c.Writer.WriteString(generateQuotaErrorHTML(limit))
			} else {
				// Return JSON for API clients
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
					"code":    429,
					"message": "Download quota exceeded. You can download up to " + strconv.Itoa(limit) + " files per 24 hours. Please login to continue.",
				})
			}
			return
		}

		// Process the request
		c.Next()

		// Only increment quota on successful responses
		status := c.Writer.Status()
		if status >= 200 && status < 300 {
			quota.Increment(connID, rangeCount)
		}
	}
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
