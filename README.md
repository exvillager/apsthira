# Apsthira 📄🔗

> **"Fluid resumes, permanent links. Securely host and update your CV via Cloudflare R2."**

Apsthira is a secure, lightweight, self-hosted web application built in Go that allows users to register accounts, upload PDF resumes, generate static links, and update or delete them from a central dashboard. 

The name **Apsthira** draws from Sanskrit roots: **Ap** (अप् - *Water/Flow*, representing the updatable nature of your resume) and **Sthira** (स्थिर - *Static/Stable*, representing the permanent link).

The PDF resumes are stored in Cloudflare R2 storage and streamed securely through the server, ensuring your storage credentials remain private.

---

## Features

- **Single Binary**: All frontend assets (HTML, CSS, JS) and backend logic compile into one standalone executable.
- **Multi-User Dashboard**: Users can sign up, log in, view all their uploaded links, copy URLs, upload replacements, or delete them.
- **Secure Sessions**: User sessions are handled securely with HTTP-only cookies mapped to a `sessions` table in SQLite.
- **Static Preview Links**: Public links (e.g. `/r/johndoe`) are completely static and simplified—the update controls are removed from the viewer and kept entirely in the dashboard.
- **Bcrypt Encryption**: User passwords are encrypted on creation using Bcrypt before being saved in SQLite.
- **Secure R2 Streaming**: Resumes are served securely via `/r/johndoe/raw` by downloading from R2 on the server side, keeping R2 tokens and bucket names hidden.
- **Strict PDF Inspection**:
  - File extension check (`.pdf`).
  - HTTP header content type verification (`application/pdf`).
  - Content sniffing (`http.DetectContentType` on the first 512 bytes) to detect and block masked malicious files.
  - Connection/request body size limitation of **10MB** strictly enforced.

---

## Tech Stack

- **Backend**: Go (using custom lightweight **nanoServe** router)
- **Database**: SQLite3 (managed automatically, requires no setup)
- **Storage**: Cloudflare R2 (S3-compatible)
- **Frontend**: HTML5, Vanilla JS, CSS3 (glassmorphic dark theme)

---

## Configuration

Copy `.env.example` to `.env` and fill in your Cloudflare R2 credentials.

```bash
cp .env.example .env
```

### Environment Variables

| Variable | Description | Default |
|---|---|---|
| `PORT` | The port the Go application runs on | `8080` |
| `DB_PATH` | Path to the SQLite database file | `resumes.db` |
| `R2_ACCOUNT_ID` | Cloudflare Account ID | *Required* |
| `R2_ACCESS_KEY_ID` | Cloudflare R2 Access Key ID | *Required* |
| `R2_SECRET_ACCESS_KEY` | Cloudflare R2 Secret Access Key | *Required* |
| `R2_BUCKET_NAME` | Cloudflare R2 Bucket Name | *Required* |

---

## Getting Started

### 1. Configure Cloudflare R2
1. Go to your **Cloudflare Dashboard** > **R2 Object Storage**.
2. Create a bucket (e.g. `apsthira-bucket`).
3. Click **Manage R2 API Tokens** > **Create API Token**:
   - Provide a name (e.g. `apsthira-token`).
   - Permissions: Select **Edit** (this allows uploading and deleting).
   - Click **Create API Token**.
4. Copy the values (`Access Key ID`, `Secret Access Key`, `Account ID`, and `Bucket Name`) into your `.env` file.

### 2. Build and Run

You can build and run Apsthira using the provided utility shell scripts:

#### Development Mode
To run the server on-the-fly (auto-compiles and starts):
```bash
./run.sh
```

#### Production Build
To run static vetting tests and compile the project into an optimized single binary:
```bash
./build.sh
```

To run the compiled production binary:
```bash
./run.sh --prod
```

The server will start on `http://localhost:8080`.

---

## How It Works

1. **Sign Up / Log In**:
   - Go to `http://localhost:8080/register` to create a new user profile.
   - Go to `http://localhost:8080/login` to authenticate.
2. **Dashboard**:
   - Create a static link by choosing a slug (e.g., `johndoe`) and dropping a PDF resume.
   - Click **Generate Static Link** to upload to R2 and list the link on your dashboard.
   - Click **Copy** to copy the static link `/r/johndoe` to your clipboard.
   - Click **Replace** to upload a new PDF and overwrite the existing one (without changing the link).
   - Click **Delete** to remove the resume from both R2 storage and the SQLite database.
3. **Public Viewer**:
   - Direct access to `/r/johndoe` serves the simplified view template showing the PDF inline.
   - All update features are restricted to the dashboard, ensuring a clean static presentation page.
