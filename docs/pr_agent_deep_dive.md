# PR-Agent Mimari Analiz Raporu

Bu doküman, PR-Agent (`/home/hermes/pr-agent/pr_agent`) kaynak kodunun derinlemesine mimari analizini içerir. Tüm modüller, araçlar (tools), prompt yapıları, git sağlayıcıları entegrasyonu ve token limit yönetimi kaynak koddan çıkarılmıştır.

---

## İçindekiler

1. [Genel Mimari](#1-genel-mimari)
2. [Araçlar (Tools) Modülleri](#2-araçlar-tools-modülleri)
3. [Prompt Yapıları ve Şablonlar](#3-prompt-yapıları-ve-şablonlar)
4. [Git Sağlayıcıları Entegrasyonu](#4-git-sağlayıcıları-entegrasyonu)
5. [Token Limit Yönetimi](#5-token-limit-yönetimi)
6. [Konfigürasyon ve Ayarlar](#6-konfigürasyon-ve-ayarlar)
7. [Çalışma Akışı](#7-çalışma-akışı)

---

## 1. Genel Mimari

### 1.1 Ana Giriş Noktası

PR-Agent'in ana giriş noktası **`cli.py`** dosyasıdır. Bu dosya:

- Komut satırı argümanlarını parse eder (`--pr_url`, `--issue_url`, `--stdin`, `--diff-file`, `--output`, `--json-output`)
- Desteklenen komutları tanımlar: `review`, `describe`, `improve`, `ask`, `update_changelog`, `add_docs`, `generate_labels`, `similar_issue`, `config`, `settings`, `help`
- `PRAgent` sınıfını başlatır ve istekleri yönlendirir

### 1.2 PRAgent Sınıfı (`agent/pr_agent.py`)

Merkezi orkestratör sınıfıdır:

```python
command2class = {
    "auto_review": PRReviewer,
    "answer": PRReviewer,
    "review": PRReviewer,
    "review_pr": PRReviewer,
    "describe": PRDescription,
    "describe_pr": PRDescription,
    "improve": PRCodeSuggestions,
    "improve_code": PRCodeSuggestions,
    "ask": PRQuestions,
    "ask_question": PRQuestions,
    "ask_line": PR_LineQuestions,
    "update_changelog": PRUpdateChangelog,
    "config": PRConfig,
    "settings": PRConfig,
    "help": PRHelpMessage,
    "similar_issue": PRSimilarIssue,
    "add_docs": PRAddDocs,
    "generate_labels": PRGenerateLabels,
}
```

Her komut bir tool sınıfına eşlenir. `PRAgent.handle_request()` metodu:
1. Repo ayarlarını uygular (`apply_repo_settings`)
2. Komut argümanlarını doğrular (`CliArgs.validate_user_args`)
3. Yanıt dilini ayarlar
4. İlgili tool sınıfını instantiate edip `.run()` metodunu çağırır

### 1.3 Temel Bileşenler

| Bileşen | Dosya | Açıklama |
|---------|-------|----------|
| **Config Loader** | `config_loader.py` | Dynaconf tabanlı config yönetimi (TOML, env, secrets) |
| **AI Handlers** | `algo/ai_handlers/` | LiteLLM tabanlı model çağrıları, fallback, streaming |
| **Token Handler** | `algo/token_handler.py` | Tiktoken tabanlı token sayımı, model-bağımlı hesaplama |
| **Token Budget** | `algo/token_budget.py` | Prompt sığdırma, chunking, output reserve hesaplamaları |
| **Git Providers** | `git_providers/` | GitHub, GitLab, Bitbucket, Azure DevOps, Gitea, Gerrit, CodeCommit |
| **Utils** | `algo/utils.py` | Markdown rendering, YAML parsing, identity markers |
| **Output Models** | `algo/output_models.py` | Pydantic modelleri (structured output validation) |
| **Repo Context** | `algo/repo_context.py` | AGENTS.md/CLAUDE.md gibi dosyaları prompt'a enjekte etme |
| **Skills Loader** | `algo/skills_loader.py` | Organizasyonel standartları/skills'leri prompt'a ekleme |

---

## 2. Araçlar (Tools) Modülleri

Tüm tool'lar `/home/hermes/pr-agent/pr_agent/tools/` altında yer alır. Her tool ortak bir kalıbı takip eder:
- `__init__`: Git provider, AI handler, token handler, prompt variables başlatır
- `run()`: Ana çalışma döngüsü (progress comment, retry_with_fallback_models, prediction, publish)
- `_prepare_prediction()`: Diff alma, token budget fitting, model çağrısı hazırlığı
- `_get_prediction()`: AI handler ile chat completion
- `_prepare_pr_answer()` / `_prepare_pr_review()`: Model çıktısını işleyip markdown üretir

### 2.1 PRReviewer (`pr_reviewer.py`) — `/review`

**En kapsamlı tool.** Kod incelemesi yapar, bulguları YAML olarak döndürür.

**Özellikler:**
- **Incremental Review** (`-i` flag): Sadece yeni commit'leri inceler
- **Large PR Chunking**: Token limiti aşıldığında diff'i parçalara böler, her parçayı ayrı review eder, sonuçları merge eder (`merge_review_chunks`)
- **Persistent Finding State**: Önceki review bulgularını takip eder (çözülmüş/yeni), durum marker'ı ile saklar
- **Inline Key Issues**: Kritik bulguları inline comment olarak yayınlar (deduplication ile)
- **Review Labels**: Effort (1-5), Security concern etiketleri ekler
- **Structured Output**: `PRReview` Pydantic modeli ile doğrulama

**Prompt Değişkenleri (`self.vars`):**
```python
{
    "title", "branch", "description", "language", "diff",
    "num_pr_files", "num_max_findings", "require_score",
    "require_tests", "require_estimate_effort_to_review",
    "require_risk_assessment", "require_merge_recommendation",
    "require_priority_files", "require_estimate_contribution_time_cost",
    "require_can_be_split_review", "require_security_review",
    "require_todo_scan", "question_str", "answer_str",
    "extra_instructions", "skills_context", "repo_context",
    "commit_messages_str", "custom_labels", "enable_custom_labels",
    "is_ai_metadata", "diff_hunk_format", "related_tickets",
    "related_tickets_omitted", "duplicate_prompt_examples", "date"
}
```

**Chunking Akışı (`_prepare_chunked_prediction`):**
1. `get_pr_multi_diffs` ile diff'i chunk'lara böler (max 3 çağrı)
2. Her chunk için async `_get_prediction` çağırır
3. Başarılı chunk'ları `merge_review_chunks` ile birleştirir
4. Başarısız chunk sayısını `review_failed_chunk_count` olarak takip eder

### 2.2 PRDescription (`pr_description.py`) — `/describe`

PR açıklaması üretir: type, title, description, file walkthrough, diagram.

**Özellikler:**
- **Semantic File Types**: Her dosya için `label` (bug fix, tests, enhancement, vb.) üretir
- **PR Diagram**: Mermaid LR/TD flowchart diagramı
- **Large PR Handling**: Çok dosyalı PR'larda önce file-level özetler, sonra header üretir
- **Markers**: `use_description_markers` ile kullanıcı açıklamasını korur
- **Labels**: `publish_labels` ile PR etiketleri ekler

**Prompt Değişkenleri:**
```python
{
    "title", "branch", "description", "language", "diff",
    "extra_instructions", "skills_context", "repo_context",
    "commit_messages_str", "enable_custom_labels", "custom_labels_class",
    "enable_semantic_files_types", "related_tickets", "related_tickets_omitted",
    "include_file_summary_changes", "duplicate_prompt_examples",
    "enable_pr_diagram", "enable_pr_description"
}
```

### 2.3 PRCodeSuggestions (`pr_code_suggestions.py`) — `/improve`

Kod iyileştirme önerileri üretir.

**Özellikler:**
- **Decoupled Hunks**: Hunk'ları bağımsız işler (varsayılan: true)
- **Scoring**: Her öneriye 0-10 skor verir, threshold ile filtreler
- **Dual Publishing**: Yüksek skorlu önerileri hem tablo hem committable olarak yayınlar
- **Persistent Comment**: Önceki önerileri günceller (edit)
- **Self-Review Checkbox**: Yazarın kendi review'ını onaylaması için checkbox
- **Incremental** (`-i`): Sadece yeni değişen dosyalar için öneri üretir
- **Code Suggestion State**: GitLab/GitHub'ta thread reconciliation (resolved/fixed)

**Prompt Değişkenleri:**
```python
{
    "title", "branch", "description", "language", "diff",
    "diff_no_line_numbers", "num_code_suggestions",
    "extra_instructions", "skills_context", "repo_context",
    "suggestion_discussion_context", "commit_messages_str",
    "relevant_best_practices", "is_ai_metadata", "diff_hunk_format",
    "focus_only_on_problems", "date", "duplicate_prompt_examples"
}
```

### 2.4 PRGenerateLabels (`pr_generate_labels.py`) — `/generate_labels`

PR için etiket önerir.

**Basit akış:**
1. Diff alır, token budget'a sığdırır
2. Modelden `Labels` modeli (string listesi) bekler
3. Mevcut kullanıcı etiketlerini koruyarak yeni etiketleri ekler

### 2.5 PRQuestions (`pr_questions.py`) — `/ask`

PR hakkında soru cevaplar.

**Özellikler:**
- **Conversation History**: Threaded soru-cevap geçmişini context'e ekler
- **Image Support**: Comment'te görsel varsa (`![image](url)`) vision modeline gönderir
- **Threaded Replies**: `comment_id` ile cevabı thread'e reply olarak atar

### 2.6 PR_LineQuestions (`pr_line_questions.py`) — `/ask_line`

Belirli bir satır aralığı hakkında soru cevaplar.

**Özellikler:**
- **Hunk Extraction**: `extract_hunk_lines_from_patch` ile istenen satırların hunk'ını çıkarır
- **Thread Resolution**: Model `[THREAD_RESOLVED]` ile biterse thread'i resolve eder
- **Conversation History**: Inline thread geçmişini yükler

### 2.7 PRAddDocs (`pr_add_docs.py`) — `/add_docs`

Kod dokümantasyonu (docstring, JSDoc, Javadoc) önerir.

**Özellikler:**
- **Language-Specific**: Python→Sphinx/Google/NumPy, Java→Javadoc, JS/TS→JSDoc, C++→Doxygen
- **Inline Suggestions**: Önerileri `suggestion` block olarak inline comment olarak yayınlar
- **Dedent Logic**: Mevcut kodun indent'ine göre öneriyi hizalar

### 2.8 PRUpdateChangelog (`pr_update_changelog.py`) — `/update_changelog`

CHANGELOG.md'yi günceller.

**Özellikler:**
- **Push Option**: `push_changelog_changes=true` ile repo'ya commit atar (`[skip ci]`)
- **Fallback**: Push yapılamazsa (restricted_mode, provider unsupported) comment olarak yayınlar
- **File Reading**: CHANGELOG.md'yi hedef branch'ten okur (ilk 50 satır)

### 2.9 PRSimilarIssue (`pr_similar_issue.py`) — `/similar_issue`

Benzer issue'ları bulur (RAG).

**Özellikler:**
- **Vector DB**: Pinecone, LanceDB, Qdrant destekler
- **Embedding**: OpenAI `text-embedding-ada-002` kullanır
- **Indexing**: Repo tüm issue'larını indexler (ilk çalışmada), sonra incremental günceller
- **Query**: Mevcut issue embedding'i ile similarity search yapar

### 2.10 PRConfig / PRHelpMessage

- **PRConfig**: Mevcut config'i gösterir / ayarlar
- **PRHelpMessage**: Kullanım kılavuzu gösterir

---

## 3. Prompt Yapıları ve Şablonlar

Tüm prompt'lar `/home/hermes/pr-agent/pr_agent/settings/*.toml` dosyalarında **Jinja2** şablonları olarak tanımlanır.

### 3.1 Prompt Yapısı

Her prompt iki kısımdan oluşur:
- **system**: Sistem mesajı (rol, kurallar, output schema)
- **user**: Kullanıcı mesajı (PR bilgileri, diff, örnekler)

### 3.2 PR Review Prompt (`pr_reviewer_prompts.toml`)

**System Prompt Yapısı:**
1. Rol tanımı: "PR-Reviewer, a language model designed to review a Git Pull Request"
2. Diff formatı: `{{ diff_hunk_format }}` (satır numaraları, AI metadata opsiyonel)
3. Review kuralları: Bug/security odaklı, spesifik/actionable, confidence-based
4. **Conditional Sections** (Jinja2 `{% if %}`):
   - `skills_context`: Organizasyonel standartlar
   - `extra_instructions`: Kullanıcı talimatları
   - `repo_context`: AGENTS.md/CLAUDE.md içeriği
5. **Output Schema**: Pydantic modeli YAML olarak tanımlanır (dinamik alanlar: `require_score`, `require_security_review`, vb.)

**User Prompt Yapısı:**
1. Related ticket bilgileri (varsa)
2. PR Info: title, branch, description, commit messages
3. Kullanıcı Q&A (varsa)
4. PR Code Diff: `{{ diff|trim }}`
5. **Duplicate Prompt Examples**: `duplicate_prompt_examples=true` ise few-shot örnek eklenir
6. Response formatı: ```yaml ... ``` block

**Dinamik Output Alanları:**
| Setting | Output Alanı | Açıklama |
|---------|-------------|----------|
| `require_score_review` | `score` (0-100) | PR kalite skoru |
| `require_tests_review` | `relevant_tests` (Yes/No) | Test var mı? |
| `require_estimate_effort_to_review` | `estimated_effort_to_review_[1-5]` | Review zorluğu |
| `require_risk_assessment` | `risk_level` (low/medium/high) | Risk seviyesi |
| `require_merge_recommendation` | `merge_recommendation` | Merge önerisi |
| `require_priority_files` | `review_priority_files` | Öncelikli dosyalar |
| `require_security_review` | `security_concerns` | Güvenlik bulguları |
| `require_todo_scan` | `todo_sections` | TODO yorumları |
| `require_can_be_split_review` | `can_be_split` | Sub-PR önerileri |
| `require_estimate_contribution_time_cost` | `contribution_time_cost_estimate` | Geliştirme süresi tahmini |

### 3.3 PR Description Prompt (`pr_description_prompts.toml`)

**Output Schema (`PRDescription`):**
- `type`: List[PRType] (Bug fix, Tests, Enhancement, Documentation, Other)
- `description`: Bullet points (1-4, max 8 words each)
- `title`: Concise title
- `changes_diagram`: Mermaid flowchart (opsiyonel)
- `pr_files`: List[FileDescription] (max 20, filename, changes_title, label, changes_summary)

### 3.4 PR Code Suggestions Prompt (`pr_code_suggestions_prompts.toml`)

**İki mod:**
- **Decoupled** (varsayılan): Her hunk bağımsız, `suggestion` block formatında
- **Not Decoupled**: Tüm dosya context'li

**Output Schema (`PRCodeSuggestions`):**
- `code_suggestions`: List[CodeSuggestion] (relevant_file, language, existing_code, suggestion_content, improved_code, one_sentence_summary, label)

### 3.5 Diğer Prompt'lar

| Dosya | Tool | Output Model |
|-------|------|--------------|
| `pr_generate_labels_prompts.toml` | generate_labels | `Labels` (string listesi) |
| `pr_questions_prompts.toml` | ask | Free text (markdown) |
| `pr_line_questions_prompts.toml` | ask_line | Free text |
| `pr_add_docs_prompts.toml` | add_docs | `CodeDocumentation` |
| `pr_update_changelog_prompts.toml` | update_changelog | Free text (markdown) |
| `pr_custom_labels.toml` | describe | Custom PRType enum |

### 3.4 Prompt Fragmentları (`prompt_fragments.toml`)

Paylaşılan diff formatı:
```toml
diff_hunk_format = """
Each file diff is presented as:
## File: 'path/to/file.py'
```diff
@@ -start_line,count +start_line,count @@
-context line
+added line
 unchanged line
```
"""
```

---

## 4. Git Sağlayıcıları Entegrasyonu

### 4.1 Base Class: `GitProvider` (`git_providers/git_provider.py`)

Soyut temel sınıf. Sağlayıcılar şu yetenekleri implement eder:

**Zorunlu Abstract Metotlar:**
- `is_supported(capability: str) -> bool`
- `get_files() -> list`
- `get_diff_files() -> list[FilePatchInfo]`
- `publish_description(pr_title, pr_body)`
- `publish_code_suggestions(code_suggestions) -> bool`
- `get_languages()`
- `get_pr_branch()`
- `get_user_id()`
- `get_pr_description_full() -> str`
- `publish_comment(pr_comment, is_temporary=False)`
- `publish_inline_comment(body, relevant_file, relevant_line_in_file, original_suggestion)`
- `publish_inline_comments(comments: list[dict])`
- `remove_initial_comment()`
- `remove_comment(comment)`
- `get_issue_comments() -> Iterable`
- `publish_labels(labels)`
- `get_pr_labels(update=False)`
- `get_commit_messages() -> str`
- `get_repo_settings()`

**Opsiyonel/Capability Metotları:**
- `supports_incremental_kind(kind)` — `/improve -i` için
- `supports_code_suggestion_state()` — Inline thread reconciliation
- `supports_threaded_pr_questions()` — `/ask` thread replies
- `supports_line_question_history()` — `/ask_line` history
- `supports_checkbox_commands()` — Self-review checkbox
- `supports_pr_chat()` — Browser extension chat
- `supports_issue_indexing()` — `/similar_issue`
- `supports_changelog_update_review()` — CHANGELOG push review
- `supports_review_finding_state()` — Persistent review state
- `supports_comment_editing()` — Persistent comment update
- `supports_html_comment_markers()` — Identity marker rendering

### 4.2 GitHub Provider (`github_provider.py`)

**En kapsamlı implementasyon.** PyGithub kullanır.

**Özellikler:**
- **App ve User Deployment**: `deployment_type = "app" | "user"`
- **Check Runs**: `publish_as_check_run=true` ile review'ları GitHub Checks olarak yayınlar
- **Incremental Review**: `get_incremental_commits` ile commit range hesaplar, `unreviewed_files_map` oluşturur
- **Incomplete Files Detection**: GitHub 3000 dosya limitini kontrol eder (`IncompletePullRequestFilesError`)
- **Reactions**: `eyes`, `+1`, `rocket` vb. reaksiyon ekler
- **Thread Resolution**: `resolve_comment_thread` desteği
- **Repo Settings**: Per-directory `.pr_agent.toml` discovery (tree API)
- **Global Settings**: Org-level `pr-agent-settings` repo'sundan config
- **Sibling Repo Context**: `repo_context_sibling_repos` allowlist ile cross-repo dosya okuma

### 4.3 GitLab Provider (`gitlab_provider.py`)

python-gitlab kullanır.

**Özellikler:**
- **Merge Request Diff**: `mr.diffs.get()` ile diff alır
- **Discussion/Thread Model**: Inline comment'lar `Discussion` objesi olarak modellenir
- **Publish as Thread**: `publish_review_as_thread`, `publish_improve_as_thread` ile resolvable discussion
- **Code Suggestions as Review**: `publish_code_suggestions_as_review=true` ile batch draft notes
- **Auto Resolve**: `auto_resolve_fixed_inline_threads`, `resolve_outdated_inline_threads`
- **Submodule Diff Expansion**: `expand_submodule_diffs` ile submodule diff'lerini genişletir
- **Reviewer Assignment**: `handle_reviewer_assignment` ile bot reviewer atandığında auto-trigger

### 4.4 Diğer Sağlayıcılar

| Sağlayıcı | Dosya | Notlar |
|-----------|-------|--------|
| **Bitbucket Cloud** | `bitbucket_provider.py` | REST API, app password auth |
| **Bitbucket Server** | `bitbucket_server_provider.py` | Self-hosted, Bearer token |
| **Azure DevOps** | `azuredevops_provider.py` | REST API, PAT auth |
| **Gitea** | `gitea_provider.py` | GitHub API uyumlu |
| **Gerrit** | `gerrit_provider.py` | SSH + REST, patch server |
| **CodeCommit** | `codecommit_provider.py` | AWS SDK, IAM auth |
| **Local** | `local_git_provider.py` | Local git repo, file-based I/O |
| **Plain Diff** | `plain_diff_provider.py` | CLI `--stdin`/`--diff-file` modu |

### 4.5 Provider Factory (`git_providers/__init__.py`)

```python
def get_git_provider():
    provider = get_settings().config.git_provider.lower()
    if provider == "github":
        from .github_provider import GithubProvider
        return GithubProvider
    elif provider == "gitlab":
        from .gitlab_provider import GitLabProvider
        return GitLabProvider
    # ... diğerleri
```

`get_git_provider_with_context()` — Starlette context'ten installation_id alır (GitHub App).

---

## 5. Token Limit Yönetimi

PR-Agent, token limitini **proaktif** ve **hiyerarşik** yönetir.

### 5.1 TokenEncoder (`algo/token_handler.py`)

- **Tiktoken** kullanır: OpenAI modelleri için `encoding_for_model`, diğerleri için `o200k_base`
- **Thread-safe** singleton cache (`_encoder_instance`, `_lock`)
- **Model-specific**: Fallback modelleri için ayrı encoder oluşturur

### 5.2 TokenHandler

```python
class TokenHandler:
    def __init__(self, pr, vars, system, user, model=None):
        self.model = model or get_settings().config.model
        self.encoder = TokenEncoder.get_token_encoder(self.model)
        self.prompt_tokens = self._get_system_user_tokens(pr, self.encoder, vars, system, user)
    
    def count_tokens(self, patch, force_accurate=False):
        encoder_estimate = len(self.encoder.encode(patch, disallowed_special=()))
        if not force_accurate:
            return encoder_estimate
        return self._get_token_count_by_model_type(patch, encoder_estimate)
```

**Model-specific accurate counting:**
- OpenAI: Tiktoken yeterli
- Anthropic (Claude): `anthropic.Anthropic().messages.count_tokens()` API çağrısı
- Diğerleri: `model_token_count_estimate_factor` (default 0.3) çarpanı ile tahmin

### 5.3 AttemptTokenBudget (`algo/token_budget.py`)

Her model denemesi için **immutable** budget nesnesi.

**Temel Hesaplamalar:**
```python
context_window = get_max_tokens(model)  # MODEL_MAX_TOKENS'dan veya config.max_model_tokens'dan
output_reserve = output_token_reserve(model, default) or default  # DEFAULT: 4096 (HARD) / 1024 (SOFT)
available_input = context_window - output_reserve - prompt_tokens - additional_reserve
```

**Sabitler:**
```python
OUTPUT_BUFFER_TOKENS_HARD_THRESHOLD = 4096  # Max completion tokens
OUTPUT_BUFFER_TOKENS_SOFT_THRESHOLD = 1024  # Güvenli minimum
MESSAGE_FRAMING_TOKEN_ALLOWANCE = 16        # Per message overhead
REPLY_FRAMING_TOKEN_ALLOWANCE = 16          # Reply overhead
DEFAULT_TRUNCATION_MARKER = "\n...(truncated)\n"
```

**Kritik Metotlar:**

1. **`fit_prompt_variable()`** — Tek bir variable'ı (örn. `diff`) token limitine sığdırır:
   - Binary search + tokenizer ile optimal truncation point bulur
   - `keep="prefix" | "suffix"` — başını mı sonunu mu koru
   - Truncation marker ekler
   - Sığmazsa `ValueError` fırlatır (fallback model tetikler)

2. **`fit_optional_text()`** — Generic optional text fitting

3. **`prepare_request()`** — Final request token sayımı (image allowance, message framing dahil)

4. **`require_input_capacity()`** — Kapasite yoksa model denemesini fail eder

### 5.4 PR Processing (`algo/pr_processing.py`)

**`get_pr_diff()`** — Diff alma ve token fitting:
```python
def get_pr_diff(git_provider, token_handler, model, **kwargs):
    # 1. Tüm diff'i al
    # 2. token_handler ile sığdır
    # 3. Sığmazsa: large_pr_handling veya chunking
```

**Large PR Handling (`pr_description.py`):**
- `enable_large_pr_handling=true` (varsayılan)
- Önce file-level özetler (`pr_description_only_files_prompts`)
- Sonra header (`pr_description_only_description_prompts`)
- Async paralel çağrılar (`async_ai_calls=true`)

**Chunking (`pr_reviewer.py`, `pr_code_suggestions.py`):**
- `enable_large_pr_chunking=true` (reviewer), `max_number_of_calls=3`
- `get_pr_multi_diffs` ile file boundary'lerde chunk'lar
- Her chunk ayrı model çağrısı, sonuçlar merge edilir
- `merge_review_chunks` / `merge_code_suggestions_chunks`

### 5.5 Config Ayarları (Token İlgili)

```toml
[config]
model = "gpt-5.6"
fallback_models = ["gpt-5.6-terra"]
max_model_tokens = 32000          # Hard cap tüm modeller için
custom_model_max_tokens = -1       # Bilinmeyen modeller için
max_output_tokens = 0              # 0 = provider default
model_token_count_estimate_factor = 0.3  # Tahmin çarpanı
image_input_token_allowance = 4096       # Görsel başına token rezervi

[pr_reviewer]
enable_large_pr_chunking = false
max_number_of_calls = 3

[pr_description]
enable_large_pr_handling = true
max_ai_calls = 4
async_ai_calls = true

[pr_code_suggestions]
max_number_of_calls = 3
parallel_calls = true
```

---

## 6. Konfigürasyon ve Ayarlar

### 6.1 Config Hiyerarşisi (Öncelik Sırası)

1. **CLI args** (`--pr_reviewer.num_max_findings=5`)
2. **Environment variables** (`PR_AGENT_PR_REVIEWER_NUM_MAX_FINDINGS=5`)
3. **Repo-local `.pr_agent.toml`** (PR'ın base branch'inde)
4. **Per-directory `.pr_agent.toml`** (monorepo, `enable_per_directory_settings=true`)
5. **Global settings** (`use_global_settings_file=true`, org-level `pr-agent-settings` repo)
6. **Extra config URL** (`--extra_config_url` veya `PR_AGENT_EXTRA_CONFIG_URL`)
7. **Defaults** (`configuration.toml`)

### 6.2 Ana Config Bölümleri

| Bölüm | Açıklama |
|-------|----------|
| `[config]` | Global ayarlar: model, git_provider, publish_output, token limits, ignore rules |
| `[pr_reviewer]` | `/review` ayarları: enable/disable features, labels, incremental, chunking |
| `[pr_description]` | `/describe` ayarları: labels, diagram, semantic types, markers |
| `[pr_questions]` | `/ask` ayarları: conversation history, threading |
| `[pr_code_suggestions]` | `/improve` ayarları: scoring, persistent, self-review, extended mode |
| `[pr_add_docs]` | `/add_docs` ayarları: doc style |
| `[pr_update_changelog]` | `/update_changelog` ayarları: push, skip_ci |
| `[github]` | GitHub-specific: deployment_type, rate limiting, check runs |
| `[gitlab]` | GitLab-specific: threads, submodules, reviewer assignment |
| `[model_routing]` | Küçük PR'ler için ucuz model routing |
| `[otel]` | OpenTelemetry telemetry |
| `[pr_similar_issue]` | Vector DB config |

### 6.3 Secrets Management

`.secrets.toml` (gitignore'd) veya environment variables:
```toml
[openai]
key = "sk-..."

[anthropic]
key = "sk-ant-..."

[github]
personal_access_token = "ghp_..."

[gitlab]
personal_access_token = "glpat-..."

[pinecone]
api_key = "..."
cloud = "aws"
region = "us-east-1"
```

`secret_providers/` — Google Cloud Secret Manager, AWS Secrets Manager desteği.

---

## 7. Çalışma Akışı

### 7.1 Standart Komut Akışı (örn: `/review`)

```
1. CLI: cli.py → parse args → PRAgent().handle_request(pr_url, ["review"])
2. PRAgent: apply_repo_settings → validate args → set response language
3. PRAgent: command2class["review"] = PRReviewer → PRReviewer(pr_url, ai_handler).run()
4. PRReviewer.__init__:
   - git_provider = get_git_provider_with_context(pr_url)
   - main_language = get_main_pr_language()
   - vars = {...} (prompt variables)
   - token_handler = TokenHandler(pr, vars, system_prompt, user_prompt)
5. PRReviewer.run():
   - init_run_details()
   - extract_and_cache_pr_tickets() → related_tickets doldurur
   - retry_with_fallback_models(_prepare_prediction, ModelType.REGULAR)
6. _prepare_prediction(model):
   - fit_related_tickets_to_prompt_budget() → tickets sığdırır
   - get_pr_diff() → diff alır, token_handler ile sığdırır
   - chunking enabled & remaining_files → _prepare_chunked_prediction()
   - else: _get_prediction(model) → model çağrısı
7. _get_prediction(model):
   - variables["diff"] = fitted diff
   - ai_handler.chat_completion(model, temperature, system, user)
8. _load_valid_review_yaml() → YAML parse + PRReview schema validation
9. _prepare_pr_review():
   - convert_to_markdown_v2() → markdown üretir
   - persistent finding state → append_review_state()
   - inline_key_issues → _publish_key_issues_as_inline_comments()
   - push_outputs() → external sinks (stdout, webhook, slack)
   - set_review_labels() → effort/security labels
10. git_provider.publish_comment() veya publish_persistent_comment()
```

### 7.2 Fallback Model Mekanizması

`retry_with_fallback_models()` (`algo/pr_processing.py`):
```python
async def retry_with_fallback_models(func, model_type=ModelType.REGULAR, ...):
    models = [get_model(model_type)] + get_settings().config.fallback_models
    for model in models:
        try:
            return await func(model)
        except Exception as e:
            if is_retryable_error(e):  # rate limit, timeout, context length
                continue
            raise
    raise last_error
```

### 7.3 Incremental Review Akışı

1. `PRReviewer.parse_incremental(args)` → `-i` flag kontrolü
2. `git_provider.get_incremental_commits(incremental)` → Provider-specific:
   - **GitHub**: `previous_review` comment bulur → commit range hesaplar → `unreviewed_files_map`
   - **GitLab**: MR discussions'dan son review comment'i bulur → `diff_refs` ile commit range
3. `_can_run_incremental_review()` → Threshold kontrolü (min commits, min minutes)
4. Diff sadece `unreviewed_files_map` dosyalarından alınır

### 7.4 Persistent Comment / Finding State

**Identity Markers (HTML comment / link reference):**
```python
PRReviewIdentity.REGULAR = "<!-- pr-agent:review:full -->"
PRReviewIdentity.INCREMENTAL = "<!-- pr-agent:review:incremental -->"
PRCodeSuggestionsIdentity.SUMMARY = "<!-- pr-agent:improve:summary -->"
```

**Akış:**
1. `publish_persistent_comment_full()` → Mevcut comment'i bul (identity marker ile)
2. Varsa: `edit_comment()` ile güncelle, header'a commit linki ekle
3. Yoksa: Yeni comment oluştur
4. **Finding State** (`_prepare_review_finding_state`):
   - Önceki state'i oku (`parse_review_state`)
   - Mevcut bulgularla reconcile et (`reconcile_review_findings`)
   - `allow_resolution=True` → Çözülmüş bulguları "resolved" işaretle
   - Yeni state'i marker içinde encode et (`append_review_state`)

### 7.5 Inline Comment Deduplication

`algo/inline_comment_dedup.py`:
- **Fingerprint**: `body_fingerprint(body)` + `code_fingerprint(file, lines)` → SHA256
- **Store**: Provider-specific (GitHub: reactions, GitLab: discussion notes) + local JSON cache
- **Publish**: Sadece yeni fingerprint'ler publish edilir, mevcutlar atlanır
- **Verification**: Publish sonrası `get_recent_inline_comment_bodies()` ile doğrulama

---

## Ek: Dosya Yapısı Özeti

```
/home/hermes/pr-agent/pr_agent/
├── agent/
│   └── pr_agent.py              # Ana orkestratör, command2class mapping
├── algo/
│   ├── ai_handlers/             # LiteLLM, base handler, callbacks
│   ├── git_patch_processing.py  # Hunk extraction, line mapping
│   ├── inline_comment_dedup.py  # Fingerprinting, store, verification
│   ├── output_models.py         # Pydantic models (PRReview, PRDescription, ...)
│   ├── pr_processing.py         # get_pr_diff, get_pr_multi_diffs, retry_with_fallback_models
│   ├── prompt_fragments.py      # Shared diff format rendering
│   ├── repo_context.py          # AGENTS.md/CLAUDE.md loading, caching
│   ├── review_finding_state.py  # Persistent finding reconciliation
│   ├── review_merge.py          # Chunk merge logic
│   ├── run_details.py           # Token usage, timing, model tracking
│   ├── skills_loader.py         # Organizational skills context
│   ├── token_budget.py          # AttemptTokenBudget, FittedPrompt
│   ├── token_handler.py         # TokenEncoder, TokenHandler
│   └── utils.py                 # Markdown, YAML, identity, formatting
├── cli.py                       # CLI entry point, arg parsing
├── cli_pip.py                   # pipx/uvx entry point
├── command_descriptions.py      # Help textleri
├── config_loader.py             # Dynaconf settings singleton
├── config_security.py           # Secret redaction, validation
├── custom_merge_loader.py       # Per-directory config merge
├── git_providers/
│   ├── __init__.py              # Factory, get_git_provider_with_context
│   ├── git_provider.py          # Abstract base class (1061 lines)
│   ├── github_provider.py       # PyGithub (2077 lines)
│   ├── gitlab_provider.py       # python-gitlab (2243 lines)
│   ├── bitbucket_provider.py
│   ├── bitbucket_server_provider.py
│   ├── azuredevops_provider.py
│   ├── gitea_provider.py
│   ├── gerrit_provider.py
│   ├── codecommit_provider.py
│   ├── local_git_provider.py
│   ├── plain_diff_provider.py
│   └── utils.py                 # Diff parsing helpers
├── identity_providers/          # OAuth, OIDC
├── log/                         # Structured logging (structlog)
├── mosaico/                     # Image generation (PR diagram)
├── secret_providers/            # GCS, AWS Secrets Manager
├── servers/                     # Webhook server (GitHub App, GitLab)
├── settings/                    # TOML config şablonları (prompts, configs)
├── telemetry/                   # OpenTelemetry (meter, tracer, shutdown)
└── tools/                       # 10 tool implementasyonu
    ├── __init__.py
    ├── pr_reviewer.py           # /review (76KB)
    ├── pr_description.py        # /describe (60KB)
    ├── pr_code_suggestions.py   # /improve (117KB)
    ├── pr_generate_labels.py    # /generate_labels (9KB)
    ├── pr_questions.py          # /ask (11KB)
    ├── pr_line_questions.py     # /ask_line (13KB)
    ├── pr_add_docs.py           # /add_docs (11KB)
    ├── pr_update_changelog.py   # /update_changelog (15KB)
    ├── pr_similar_issue.py      # /similar_issue (33KB)
    ├── pr_config.py             # /config
    ├── pr_help_message.py       # /help
    ├── progress_comment.py      # Progress comment builder
    └── ticket_pr_compliance_check.py  # Ticket extraction/compliance
```

---

## Özet

PR-Agent, **modüler, provider-agnostik, token-budget-aware** bir mimariye sahiptir:

1. **Tool-based Architecture**: Her komut (`/review`, `/describe`, `/improve`...) bağımsız bir tool sınıfıdır, ortak base yoktur ama ortak kalıp (init → run → prepare_prediction → get_prediction → prepare_output → publish) takip eder.

2. **Prompt-as-Configuration**: Tüm prompt'lar TOML'de Jinja2 şablonlarıdır, `{% if setting %}` ile koşullu alanlar/çıkışlar desteklenir. Output schema Pydantic modelleriyle tanımlanır ve runtime'da validate edilir.

3. **Multi-Provider Abstraction**: `GitProvider` abstract base class ile 9+ git platformu desteklenir. Capability-based design (`is_supported()`, `supports_*()`) ile provider farkları yönetilir.

4. **Sophisticated Token Management**: `TokenHandler` + `AttemptTokenBudget` ile her model denemesi için ayrı, immutable budget. Diff truncation (prefix/suffix), chunking, large PR handling, fallback models ile context window sınırları proaktif yönetilir.

5. **Stateful Operations**: Persistent comments (edit-in-place), finding state reconciliation, inline comment deduplication, incremental review — hepsi provider API'larına uyumlu, idempotent tasarlanmıştır.

6. **Extensibility**: Skills loader, repo context, custom labels, model routing, secret providers, telemetry — hepsi pluggable modüllerle genişletilebilir.

Bu mimari, PR-Agent'in hem GitHub Actions/GitLab CI gibi otomasyonlarda hem de CLI/manuel kullanımda güvenilir, ölçeklenebilir ve özelleştirilebilir çalışmasını sağlar.