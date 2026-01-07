# boxbox

A code execution platform that provides on-demand sandboxed execution for Python and Bash using [Daytona](https://daytona.io) built specifically for chat applications.

## Requirements

- Go 1.25+
- PostgreSQL
- S3-compatible storage
- Daytona API access

## How it Works

boxbox isolates code execution per user and chat session using a simple file namespace:

```
s3://bucket/{user_id}/{chat_id}/
├── input.csv      # Files uploaded by user
├── output.png     # Files created by code
└── ...
```

**Flow:**
1. User sends code to execute with optional input files
2. boxbox claims a warm sandbox from the pool
3. Input files are synced from S3 → sandbox
4. Code runs in isolation (Python/Bash)
5. New/modified files are synced sandbox → S3
6. Sandbox is destroyed, results returned

**Sandbox Pool:**
- Pre-warms sandboxes for fast execution (no cold start)
- Scales up/down based on demand
- Each sandbox is single-use and destroyed after execution

**Custom Image:**
- If `DAYTONA_IMAGE` is not set, boxbox reads `image/Dockerfile` and sends it to Daytona for dynamic builds
- Pre-install packages by editing `image/Dockerfile`

## Configuration

Set the following environment variables:

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | Yes | | PostgreSQL connection string |
| `DAYTONA_API_KEY` | Yes | | Daytona API key |
| `S3_BUCKET` | Yes | | S3 bucket name |
| `API_KEY` | Yes | | API key for authentication |
| `DAYTONA_API_URL` | No | `https://app.daytona.io/api` | Daytona API URL |
| `DAYTONA_IMAGE` | No | `daytonaio/ai-python-3.12:latest` | Docker image for sandboxes |
| `S3_REGION` | No | `us-east-1` | S3 region |
| `S3_ACCESS_KEY_ID` | No | | S3 access key |
| `S3_SECRET_ACCESS_KEY` | No | | S3 secret key |
| `S3_ENDPOINT` | No | | Custom S3 endpoint (for MinIO, etc.) |
| `POOL_MIN_WARM` | No | `5` | Minimum warm sandboxes |
| `POOL_MAX_WARM` | No | `50` | Maximum warm sandboxes |
| `API_PORT` | No | `8080` | HTTP server port |
| `EXECUTION_TIMEOUT` | No | `5m` | Default execution timeout |

## Usage

### Build and Run

```bash
go build -o boxbox ./cmd/boxbox
./boxbox
```

### API Endpoints

#### Health Check
```
GET /health
```

#### Execute Code
```
POST /execute
Headers:
  X-API-Key: <api-key>
  X-User-ID: <user-id>
  X-Chat-ID: <chat-id>

Body:
{
  "language": "python",
  "path": "script.py",
  "code": "print('hello')",
  "files": ["data.csv"],
  "timeout_seconds": 60
}
```

#### Get Execution
```
GET /executions/{id}
Headers:
  X-API-Key: <api-key>
  X-User-ID: <user-id>
  X-Chat-ID: <chat-id>
```

### Example Tool

```typscript
export const execute = tool({
	description: `Execute Python or Bash in an isolated sandbox.

CRITICAL: The sandbox is completely isolated with NO access to conversation files by default. You MUST include every file you need in the 'files' parameter array, or you will get "FileNotFoundError".

Example - to process "photo.jpg" uploaded by the user:
  files: ["photo.jpg"]
  code: img = Image.open("photo.jpg")  # Now works because file was synced

Use for: data analysis, visualizations, image/audio/video processing, document generation.

Available: numpy, pandas, scipy, scikit-learn, matplotlib, seaborn, plotly, pillow, opencv, ffmpeg, moviepy, pydub, weasyprint, reportlab, python-docx, openpyxl, python-pptx, PyPDF2, beautifulsoup4, requests.`,
	inputSchema: z.object({
		language: z.enum(['python', 'bash']),
		code: z.string().max(50000),
		path: z.string().optional().describe('Filename for the code (default: main.py or script.sh).'),
		files: z
			.array(z.string())
			.optional()
			.describe(
				'Files to sync into sandbox. REQUIRED if your code reads any files - without this, files do not exist in the sandbox.'
			),
		timeout: z
			.number()
			.int()
			.min(5)
			.max(300)
			.optional()
			.describe('Timeout in seconds (default 60).')
	}),
	execute: async (
		{ language, code, path, files, timeout },
		{ experimental_context }
	): Promise<ExecuteResult> => {
		try {
			const ctx = getToolContext(experimental_context);
			const codePath = path ?? (language === 'python' ? 'main.py' : 'script.sh');

			const result = await executeInSandbox({
				userId: ctx.userId,
				chatId: ctx.chatId,
				language,
				code,
				path: codePath,
				files,
				timeout
			});

			const outputFiles = result.outputFiles.map((file) => ({
				path: file.path,
				url: getFileUrl(ctx.chatId, file.path),
				mediaType: file.media_type,
				size: file.size
			}));

			return {
				success: result.exitCode === 0,
				stdout: result.stdout || undefined,
				stderr: result.stderr || undefined,
				exitCode: result.exitCode,
				outputFiles: outputFiles.length > 0 ? outputFiles : undefined,
				executionTime: result.executionTime
			};
		} catch (error) {
			const message = error instanceof Error ? error.message : 'Unknown error occurred';
			return { success: false, error: message };
		}
	}
});
```

## License

MIT
