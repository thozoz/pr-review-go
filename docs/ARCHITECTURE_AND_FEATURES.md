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
       │   (Geçici klon; derleme/       (Yorum Geçmişi)          └─► Tool: list_files
       │    test devre dışı)
       └─► pkg/llm (LiteLLM / OpenAI)
           (Structured JSON & Promptlar)
```

---

## 2. Mevcut Özellikler ve Çalışma Mantıkları

### A. Kod İnceleme (`pkg/reviewer`, `pkg/sandbox`)
- **Tetikleyici:** PR açıldığında (`opened`), yeni commit pushlandığında (`synchronize`) veya yetkili kullanıcı yoruma `/review` yazdığında.
- **Mantık:**
  1. Balanced modda PR dalı host üzerinde geçici dizine `git clone` ile çekilir; bu işlem container izolasyonu sağlamaz.
  2. Proje tipi algılanır (`go.mod`, `package.json`, `Cargo.toml`, `pyproject.toml`).
  3. Container izolasyonu kurulana kadar PR derleme ve test komutları çalıştırılmaz; doğrulama atlandı bilgisi rapora girer.
  4. İnceleme bitince geçici dizin silinir (`defer cleanup()`).

### B. Repo Özel Kuralları Taraması (`Custom Instructions Scanner`)
- **Konum:** `pkg/sandbox/runner.go` -> `extractCustomRules()`
- **Mantık:**
  - Repo içinde ve `.github/` klasöründe dinamik tarama yapar.
  - Öncelikli: `.github/copilot-instructions.md`, `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING.md`, `.cursorrules`.
  - Heuristic Tarayıcı: Adında `instruct`, `guide`, `rule`, `contribut`, `agent`, `standard`, `convention` geçen tüm markdown dosyalarını yakalar.
  - Yakalanan içerik güvenilmeyen PR verisidir; reviewer system/user prompt'una talimat olarak eklenmez.

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

### F. Salt Okunur İnteraktif Asistan (`pkg/assistant` - `@bot`)
- **Tetikleyici:** Yazma iznine sahip kullanıcı PR yorumunda `@bot <soru>`, `@pr-review <soru>` veya `/ask <soru>` yazdığında.
- **Mantık (Tool-Calling Loop):**
  - PR dalını geçici çalışma dizinine çeker.
  - `read_file(path)` ve `list_files(path)` ile dosyaları inceler; `answer` ile yanıt verir.
  - `write_file`, `run_command` ve `commit_and_push` çağrıları reddedilir; dosya değişikliği veya push yapmaz.
  - En fazla 6 araç adımından sonra PR yorumuna yanıtını bırakır.

### G. SHA-256 Yorum Deduplication (`pkg/dedup`)
- **Mantık:**
  - Her bir bulgu (finding) için deterministik SHA-256 parmak izi hesaplar: `sha256(file:line:title:severity)`.
  - İnceleme yorumu oluşturulurken her bulgunun altına gizli bir HTML parmak izi etiketi gömer (`<!-- pr-review-go:fingerprint=... -->`).
  - PR yeniden incelendiğinde mevcut yorumlardaki tüm parmak izlerini tarar ve daha önce raporlanmış mükerrer bulguları filtreler.

### H. Otomatik Dokümantasyon Üretici (`/add_docs`, `pkg/docgen`)
- **Tetikleyici:** Yoruma `/add_docs` veya `/docs` yazıldığında ya da CLI `-add-docs` bayrağıyla.
- **Mantık:**
  - Go AST parser (`go/parser`, `go/ast`) kullanarak PR kodundaki üst düzey fonksiyon ve tip tanımlarını tarar.
  - Öncesinde doc comment (`d.Doc == nil`) bulunmayan tanımları listeler ve standart godoc şablonu üretip PR'a raporlar.

### I. Hız Seviyeleri: Lite vs Balanced (`pkg/config`, `pkg/reviewer`)
- **`lite` Modu:** Hızlı diff ve meta veri tabanlı inceleme; sandbox klonlama ve test koşumunu atlayarak saniyeler içinde review üretir.
- **`balanced` Modu (Varsayılan):** Kodu geçici çalışma dizinine çeker; container izolasyonu kurulana kadar derleme ve testleri atlar.

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
