# pr-review-go: Mimari, Özellikler ve Çalışma Mantığı

Bu doküman, `pr-review-go` projesinin tüm mimari bileşenlerini, eklenen yeteneklerini, veri akışını ve çalışma prensiplerini detaylandıran yaşayan teknik şartnamedir.

---

## 1. Genel Mimari Bakış

```
                          [GitHub PR / Webhook]
                                    │
                                    ▼
                         ┌───────────────────────┐
                         │  pkg/server (Port 3000)│
                         │  - HMAC Doğrulama     │
                         │  - Asenkron Goroutine │
                         └──────────┬────────────┘
                                    │
       ┌────────────────────────────┼────────────────────────────┐
       ▼                            ▼                            ▼
┌──────────────┐             ┌──────────────┐             ┌──────────────┐
│ pkg/reviewer │             │pkg/summarizer│             │pkg/assistant │
│(Ana İnceleme)│             │ (/summarize) │             │    (@bot)    │
└──────┬───────┘             └──────┬───────┘             └──────┬───────┘
       │                            │                            │
       ├─► pkg/sandbox              └─► GitHub API               ├─► Tool: read_file
       │   (Klon, Test, Linter,         (Yorum Geçmişi)          ├─► Tool: write_file
       │    Kural Dosyası Taraması)                              ├─► Tool: run_command
       │                                                         └─► Tool: commit_and_push
       └─► pkg/llm (LiteLLM / OpenAI)
           (Structured JSON & Promptlar)
```

---

## 2. Mevcut Özellikler ve Çalışma Mantıkları

### A. Sandbox Doğrulamalı Kod İnceleme (`pkg/reviewer`, `pkg/sandbox`)
- **Tetikleyici:** PR açıldığında (`opened`), yeni commit pushlandığında (`synchronize`) veya yoruma `/review` yazıldığında.
- **Mantık:**
  1. PR dalı geçici bir `/tmp/pr-review-sandbox-*` dizinine (veya Podman container'ına) `git clone` ile çekilir.
  2. Proje tipi otomatik algılanır (`go.mod`, `package.json`, `Cargo.toml`, `pyproject.toml`).
  3. Gerçek derleyici ve testler çalıştırılır (`go build`, `go test -race`, `cargo check`, `pytest` vb.).
  4. Çıkan hata logları veya "başarılı" sinyali LLM'e kanıt olarak verilir.
  5. **Sonuç:** LLM çalışan koda "burası bozuk" diyemez (halüsinasyon sıfırlanır).
  6. İnceleme bittiğinde geçici dizin diskten tamamen silinir (`defer cleanup()`).

### B. Repo Özel Kuralları Taraması (`Custom Instructions Scanner`)
- **Konum:** `pkg/sandbox/runner.go` -> `extractCustomRules()`
- **Mantık:**
  - Repo içinde ve `.github/` klasöründe dinamik tarama yapar.
  - Öncelikli: `.github/copilot-instructions.md`, `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING.md`, `.cursorrules`.
  - Heuristic Tarayıcı: Adında `instruct`, `guide`, `rule`, `contribut`, `agent`, `standard`, `convention` geçen tüm markdown dosyalarını yakalar.
  - Yakalanan kurallar LLM'in sistem promptuna `CRITICAL: You MUST strictly enforce the project's repository custom instructions` olarak enjekte edilir.

### C. Tartışma ve Yorum Takibi (`Discussion Memory`)
- **Konum:** `pkg/github/client.go` -> `GetComments()`
- **Mantık:**
  - GitHub API üzerinden hem PR genel yorumlarını (`IssueComments`) hem de koda özel inline yorumları (`ReviewComments`) çeker.
  - Sayfalama (pagination) ile tüm yorumları eksiksiz toplar.
  - Aynı dosya ve satırdaki yorumları `DiscussionThread` hiyerarşisinde gruplar.
  - Modele vererek: "Daha önce tartışılmış ve yazar tarafından gerekçelendirilmiş konuları tekrar gündeme getirme, açık kalan talepleri kontrol et" talimatını uygular.

### D. Otomatik Etiketleme (`pkg/labeler`)
- **Tetikleyici:** PR açılışında veya yoruma `/labels` / `/generate_labels` yazıldığında, ya da CLI `-labels` bayrağıyla.
- **Mantık:**
  - Sıfır sandbox ve sıfır kod indirme maliyeti.
  - Sadece PR başlığı, açıklaması ve API diff'ini okur.
  - Pydantic/Enum benzeri yapılandırılmış JSON çıktısıyla `Bug fix`, `Enhancement`, `Tests`, `Documentation`, `Refactoring`, `Maintenance` etiketlerini seçer.
  - GitHub API (`Issues.AddLabelsToIssue`) ile PR'a basar.

### E. Tartışma Özeti (`pkg/summarizer`)
- **Tetikleyici:** Yoruma `/summarize` veya `/summary` yazıldığında, ya da CLI `-summary` bayrağıyla.
- **Mantık:**
  - Sandbox gerektirmez; tamamen GitHub yorum geçmişi üzerinden çalışır.
  - Tüm incelemeci ve yazar mesajlarını sentezler:
    - *Uzlaşılan Kararlar (Consensus)*
    - *Açık Kalan Endişeler (Open Concerns)*
    - *Aksiyon Maddeleri (Action Items)*
  - PR altına yönetici özeti olarak basar.

### F. Otonom İnteraktif Kodlama Ajanı (`pkg/assistant` - `@bot`)
- **Tetikleyici:** PR yorumunda `@bot <talimat>`, `@pr-review <talimat>` veya `/ask <talimat>` yazıldığında.
- **Mantık (Tool-Calling Loop):**
  - PR dalını sandbox'a çeker.
  - Modelin kullanımına 4 temel araç sunar:
    1. `read_file(path)`: Dosya içeriğini okur.
    2. `write_file(path, content)`: Dosyayı günceller / sıfırdan yazar.
    3. `run_command(cmd)`: İlgili dizinde terminal komutu koşturur (test, linter, git vb.).
    4. `commit_and_push(commit_msg)`: Yapılan değişiklikleri `pr-review-go[bot]` adıyla PR dalına yeni commit olarak pushlar.
  - Model kullanıcı talimatına göre 6 adıma kadar otonom iterasyon yapabilir; işi bitince PR yorumuna açıklamasını bırakır.

---

## 3. Yol Haritası ve Eklenecek Özellikler

1. **Resmi GitHub Review API & Inline Yorumlar:**
   - Genel yorum yerine her bulguyu dosyanın ilgili satırına inline basma.
   - PR skoruna göre resmi `APPROVE` veya `REQUEST_CHANGES` kararı verme.
2. **PR Açıklaması & Mermaid Şeması (`/describe`):**
   - PR diff'inden veri akışını gösteren otomatik mimari Mermaid blok şeması çizme.
3. **SHA-256 Yorum Deduplication:**
   - Önceden basılmış bulguların parmak izini tutarak tekrarlanan yorumları filtreleme.
4. **Otomatik Dokümantasyon (`/add_docs`):**
   - Açıklamasız fonksiyonlara dil standartlarında docstring / godoc üretip commit atma.
