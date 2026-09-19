# GitHub Copilot PR Yaşam Döngüsü Yetenekleri ve Çalışma Prensipleri

Bu rapor, GitHub Copilot'un Pull Request (PR) yaşam döngüsündeki **tüm yeteneklerini**, **çalışma prensiplerini**, **mimarisi** ve **kullanım senaryolarını** resmi GitHub/Microsoft kaynaklarına dayanarak dokümante eder.

---

## İçindekiler

1. [Genel Bakış](#genel-bakış)
2. [PR Aşamaları ve Copilot Yetenekleri](#pr-aşamaları-ve-copilot-yetenekleri)
   - [2.1 PR Oluşturma ve Açıklama Oluşturma](#21-pr-oluşturma-ve-açıklama-oluşturma)
   - [2.2 PR Özeti (Summary) Oluşturma](#22-pr-özeti-summary-oluşturma)
   - [2.3 Kod İncelemesi (Code Review)](#23-kod-incelemesi-code-review)
   - [2.4 Inline Öneri Uygulama ve Otomatik Düzeltme](#24-inline-öneri-uygulama-ve-otomatik-düzeltme)
   - [2.5 @copilot Sohbeti ve Cloud Agent Etkileşimi](#25-copilot-sohbeti-ve-cloud-agent-etkileşimi)
   - [2.6 Merge Çatışması Çözümü](#26-merge-çatışması-çözümü)
   - [2.7 Güvenlik Açığı Otomatik Düzeltmesi (Copilot Autofix)](#27-güvenlik-açığı-otomatik-düzeltmesi-copilot-autofix)
   - [2.8 Test ve CI/CD Entegrasyonu](#28-test-ve-cicd-entegrasyonu)
3. [Arkadaki Mimari ve Çalışma Prensipleri](#arkadaki-mimari-ve-çalışma-prensipleri)
   - [3.1 Agentic Mimari](#31-agentic-mimari)
   - [3.2 GitHub Actions Runner Kullanımı](#32-github-actions-runner-kullanımı)
   - [3.3 Context Toplama (Context Gathering)](#33-context-toplama-context-gathering)
   - [3.4 Copilot Memory](#34-copilot-memory)
   - [3.5 Model Kullanımı](#35-model-kullanımı)
4. [Özelleştirme ve Yapılandırma](#özelleştirme-ve-yapılandırma)
   - [4.1 Custom Instructions](#41-custom-instructions)
   - [4.2 Agent Skills](#42-agent-skills)
   - [4.3 Review Effort Level (Lite vs Balanced)](#43-review-effort-level-lite-vs-balanced)
   - [4.4 Otomatik Kod İncelemesi Yapılandırması](#44-otomatik-kod-incelemesi-yapılandırması)
5. [Maliyet ve Faturalandırma](#maliyet-ve-faturalandırma)
6. [Sınırlılıklar ve Bilinen Sorunlar](#sınırlılıklar-ve-bilinen-sorunlar)
7. [Kaynaklar](#kaynaklar)

---

## Genel Bakış

GitHub Copilot, PR yaşam döngüsünün **her aşamasında** geliştiricilere yardımcı olan çok yönlü bir AI asistanıdır. Temel bileşenleri şunlardır:

| Bileşen | Açıklama | Kullanılabilirlik |
|---------|----------|-------------------|
| **Copilot Code Review** | PR'leri inceleyen, sorun tespit eden, düzeltme öneren agent | Tüm ücretli Copilot planları |
| **Copilot Cloud Agent** | PR'ler üzerinde autonome çalışan, kod değiştiren, test çalıştıran agent | Tüm ücretli Copilot planları |
| **Copilot Chat** | PR bağlamında soru-cevap, inceleme, özetleme yapan chat arayüzü | Tüm Copilot planları |
| **Copilot Autofix** | CodeQL güvenlik uyarıları için otomatik düzeltme PR'i oluşturan sistem | GitHub Advanced Security |
| **PR Summaries** | PR değişikliklerinin AI destekli özetini oluşturan özellik | Copilot Enterprise/Pro+ |

> **Not**: Copilot Free planında PR özeti, kod incelemesi ve cloud agent özellikleri **yoktur**.

---

## PR Aşamaları ve Copilot Yetenekleri

### 2.1 PR Oluşturma ve Açıklama Oluşturma

**Kaynak**: [Microsoft DevBlogs - "Let GitHub Copilot draft your pull request description"](https://devblogs.microsoft.com/visualstudio/let-github-copilot-draft-of-your-pull-request-description/), [GitHub Next - "Copilot for Pull Requests"](https://githubnext.com/projects/copilot-for-pull-requests/)

**Yetenekler**:
- **VS Code / Visual Studio / GitHub Web UI** üzerinden PR oluştururken "Add AI Generated Pull Request Description" (✨ sparkle pen ikonu) ile tek tıkla PR açıklaması taslağı oluşturma
- Commit mesajlarından ve kod değişikliklerinden (diff) yararlanarak anlamlı açıklama üretme
- Markdown formatında çıktı verme, geliştiricinin düzenlemesine imkan tanıma
- **Insertion rate**: %80 oranında geliştiriciler önerilen açıklamayı kabul ediyor (Microsoft verisi)

**Nasıl Çalışır**:
1. Geliştirici "Create Pull Request" ekranına gelir
2. Açıklama alanındaki Copilot ikonuna (✨) tıklar
3. Copilot diff'i analiz eder, commit mesajlarını okur
4. Yapılandırılmış bir PR açıklaması taslağı üretir (ne değiştirildiği, neden, etkileri)
5. Geliştirici taslağı inceleyip, düzenleyip kaydeder

**GitHub Next Dönemindeki Marker Sistemi** (eski, artık PR Summaries ile değiştirildi):
- `copilot:all` - Tüm içerik türlerini gösterir
- `copilot:summary` - Tek paragraf özet
- `copilot:walkthrough` - Kod linkleriyle detaylı değişiklik listesi
- `copilot:poem` - Değişiklikler hakkında şiir (eğlence amaçlı)

---

### 2.2 PR Özeti (Summary) Oluşturma

**Kaynak**: [GitHub Docs - "Creating a pull request summary with GitHub Copilot"](https://docs.github.com/enterprise-cloud@latest/copilot/github-copilot-enterprise/copilot-pull-request-summaries/creating-a-pull-request-summary-with-github-copilot), [GitHub Blog - "Copilot Chat improvements for pull requests"](https://github.blog/changelog/2026-04-23-copilot-chat-improvements-for-pull-requests/)

**Yetenekler**:
- PR açıklama alanında **veya** yorum alanında (alt kısım) özet oluşturma
- "Summary" komutu ile tek tıkta özet üretme
- Değişikliklerin ne olduğu, hangi dosyaların etkilendiği, review odaklanması gereken alanlar
- Prose (düz metin) + madde işaretli liste formatında çıktı

**Kullanım**:
1. PR oluşturma sayfasında veya var olan PR'de açıklama/yorum alanına tıkla
2. Metin alanının başlığındaki Copilot ikonunu seç → **Summary**'ye tıkla
3. Oluşan özeti incele, ek bağlam ekle, kaydet

**Önemli Not**: Copilot, PR açıklamasındaki **mevcut içeriği dikkate almaz**; en iyi sonuç için boş açıklama ile başlamak önerilir.

**Copilot Chat ile Özet Alma** (2026 Nisan güncellemesi):
- GitHub.com/copilot veya global Copilot navigasyonu üzerinden PR linki vererek "Summarize this pull request" demek yeterlidir
- PR anlayışı: Yorumlar, dosya değişiklikleri, commit'ler, incelemeler dahil edilir

---

### 2.3 Kod İncelemesi (Code Review)

**Kaynak**: [GitHub Docs - "About GitHub Copilot code review"](https://docs.github.com/copilot/concepts/agents/code-review), [GitHub Blog - "60 million Copilot code reviews and counting"](https://github.blog/ai-and-ml/github-copilot/60-million-copilot-code-reviews-and-counting/), [GitHub Blog - "Copilot code review now runs on an agentic architecture"](https://github.blog/changelog/2026-03-05-copilot-code-review-now-runs-on-an-agentic-architecture)

**Temel Yetenekler**:
- **Herhangi bir dildeki kodu** inceleme
- Çoklu açıdan analiz: güvenlik, performans, kod kalitesi, mimari uygunluk, test eksiklikleri
- **Inline öneri** (suggested changes) ile tek tıkta uygulanabilir düzeltmeler
- **Overview comment** (genel bakış yorumu) ile PR düzeyinde değerlendirme ve onay/red kararı
- **Batch autofix**: Aynı tip hataları (örn. stil, mantık hataları) toplu halde düzeltme

**İnceleme Modları**:
| Mod | Açıklama | Kullanım Alanı |
|-----|----------|----------------|
| **Manual Review** | Geliştirici PR'ye Copilot'ı reviewer olarak atar | İsteğe bağlı inceleme |
| **Automatic Review** | Repo/organizasyon ayarında her PR için otomatik tetiklenir | Sürekli entegrasyon, politikalar |
| **Bot-authored PR Review** | Copilot Cloud Agent'ın açtığı PR'ler de tam inceleme alır (Ağustos 2026+) | Agent üretimi PR'ler |

**Agentic Mimari Avantajları** (Mart 2026+):
- Repository context'i aktif toplar (ilgili kod, dizin yapısı, referanslar)
- Değişikliklerin büyük mimariye nasıl sığdığını anlar
- Daha az gürültü, daha yüksek sinyal (actionable feedback)
- Uzun PR'ler için explicit plan yapar, context kaybını önler
- Bağlantılı issue/PR'leri okuyarak gereksinim uygunluğunu kontrol eder
- **71% oranında** actionable feedback üretir, ortalama **5.1 yorum/review**

**Onay (Approval) Mekanizması**:
- Her incelemede overview comment'te "approval assessment" yer alır
- Varsayılan: Copilot onayları **required approval** sayılmaz
- Yapılandırılarak "Count Copilot approvals toward merge requirements" aktif edilebilir

---

### 2.4 Inline Öneri Uygulama ve Otomatik Düzeltme

**Kaynak**: [GitHub Docs - "Using GitHub Copilot code review"](https://docs.github.com/copilot/using-github-copilot/code-review/using-copilot-code-review), [AugmentCode - "GitHub AI Code Review: 8 Copilot PR Automation Features"](https://www.augmentcode.com/tools/github-copilot-ai-code-review)

**İki Farklı Otomatik Düzeltme Sistemi**:

#### A. Code Review Suggestion Implementation (Genel Kod Kalitesi)
- Copilot code review yorumlarında **"Implement suggestion"** butonu
- Geliştirici tıkladığında: draft comment oluşturur, Copilot'a yönlendirme verir
- Yeni PR (branch'e karşı) **veya** aynı PR'e commit olarak eklenebilir
- Her uygulama geliştirici onayı gerektirir (manual trigger)

#### B. Copilot Autofix for Code Scanning (Güvenlik Odaklı) - Bölüm 2.7'de detaylı

**Inline Suggestion Uygulama Akışı**:
1. Copilot review yorumu altındaki "Implement suggestion" / "Fix with Copilot" butonuna tıkla
2. Draft comment açılır, geliştirici isteği netleştirir (opsiyonel)
3. "Create new pull request" veya "Commit to same pull request" seç
4. Copilot cloud agent sandbox'ta değişikliği yapar, testleri çalıştırır
5. Sonuç PR olarak gelir, geliştirici review/merge eder

---

### 2.5 @copilot Sohbeti ve Cloud Agent Etkileşimi

**Kaynak**: [GitHub Docs - "Using Copilot cloud agent on GitHub"](https://docs.github.com/en/copilot/how-tos/use-copilot-agents/cloud-agent/use-cloud-agent-on-github), [GitHub Blog - "Copilot Chat improvements for pull requests"](https://github.blog/changelog/2026-04-23-copilot-chat-improvements-for-pull-requests)

**@copilot Mention Kullanım Alanları**:
| Komut Örneği | Açıklama |
|--------------|----------|
| `@copilot Address this comment` | Review yorumunu adresle, kodu düzelt |
| `@copilot Fix the failing tests` | Başarısız GitHub Actions workflow/testleri düzelt |
| `@copilot Merge in main and resolve the conflicts` | Merge çatışmasını çöz |
| `@copilot Add a unit test covering...` | Yeni test ekle, özellik implement et |
| `@copilot Summarize this PR` | PR özeti çıkar (Chat'te) |
| `@copilot Review this PR` | Yapılandırılmış code review yap (Chat'te) |

**Cloud Agent Oturum Başlatma Noktaları**:
1. **Agents Tab/Panel** → "New agent task" formu
2. **Dashboard** → "Task" butonu
3. **Copilot Chat** → `/task` komutu (chat context'i taşır)
4. **Issue Atama** → Issue'assignee olarak "Copilot" seç
5. **PR Yorumunda** → `@copilot` mention
6. **Failed Workflow** → "Fix with Copilot" butonu
7. **Merge Box** → "Fix with Copilot" butonu (merge conflict için)
8. **Mobile App** → PR yorumunda "Fix with Copilot"

**Cloud Agent Çalışma Prensibi**:
- **Ephemeral GitHub Actions ortamında** çalışır (sandbox)
- Repo'yu klonlar, kodları okur, plan yapar, değişiklik yapar
- Test/linter çalıştırır, build doğrular
- Değişiklikleri branch'e push eder, PR açar/günceller
- Geliştirici review eder, merge eder
- **Context paylaşımı**: Aynı PR üzerindeki önceki oturumları hatırlar, follow-up hızlı olur

---

### 2.6 Merge Çatışması Çözümü

**Kaynak**: [GitHub Blog - "Ask @copilot to resolve merge conflicts on pull requests"](https://github.blog/changelog/2026-03-26-ask-copilot-to-resolve-merge-conflicts-on-pull-requests), [GitHub Blog - "Fix merge conflicts in three clicks with Copilot cloud agent"](https://github.blog/changelog/2026-04-13-fix-merge-conflicts-in-three-clicks-with-copilot-cloud-agent)

**İki Yöntem**:
1. **@copilot mention**: PR yorumuna `@copilot Merge in main and resolve the conflicts` yaz
2. **Merge Box Butonu**: PR'nin merge kutusundaki "Fix with Copilot" butonu → hazır comment'i gönder

**Süreç**:
- Cloud agent kendi ortamında conflict'leri çözer
- Build ve testlerin geçtiğini doğrular
- Çözümü push eder, review ister
- Mobile uygulamada da kullanılabilir

---

### 2.7 Güvenlik Açığı Otomatik Düzeltmesi (Copilot Autofix)

**Kaynak**: [Microsoft Learn - "Copilot Autofix for code scanning (Preview)"](https://learn.microsoft.com/en-us/azure/devops/repos/security/github-advanced-security-code-scanning-autofix?view=azure-devops), [GitHub Blog - "Copilot code review now runs on an agentic architecture"](https://github.blog/changelog/2026-03-05-copilot-code-review-now-runs-on-an-agentic-architecture)

**Özellikler**:
- **CodeQL** taraması sonucu çıkan güvenlik uyarıları (vulnerabilities) için
- "Generate fix" butonu → Copilot coding agent analiz eder, düzeltme üretir
- `copilot-autofix/...` branch'i açar, PR oluşturur
- PR açıklamasında: uyarı ID, severity, fix detayları
- **Copilot Autofix** etiketi ile filtrelenebilir
- Merge sonrası bir sonraki CodeQL run'unda uyarı kapanır

**Desteklenen Diller**: C/C++, C#, Go, Java/Kotlin, JavaScript/TypeScript, Python, Ruby, Swift (CodeQL dilleri)

**Önemli Kısıtlamalar**:
- Her uyarı için fix **garanti değildir** (false positive olabilir, karmaşık olabilir)
- Fix üretilmezse PR oluşturulmaz, manuel müdahale gerekir
- Commit author email: `noreply@dev.azure.com` (policy engelleyebilir)
- **Limited public preview** (Azure DevOps için), GitHub.com'da GA

**Code Review vs Autofix Farkı**:
| Özellik | Copilot Code Review | Copilot Autofix |
|---------|---------------------|-----------------|
| Odak | Genel kod kalitesi, best practice | Güvenlik açıkları (CodeQL alerts) |
| Tetikleme | Manuel/otomatik PR review | CodeQL alert detay sayfasından "Generate fix" |
| Çıktı | Review yorumları, inline suggestions | Tam PR (branch + commit + description) |
| Kapsam | Tüm değiştirilen dosyalar | Alert'ın etrafındaki context + ilgili dosyalar |

---

### 2.8 Test ve CI/CD Entegrasyonu

**Kaynak**: [GitHub Docs - "Using Copilot cloud agent on GitHub"](https://docs.github.com/en/copilot/how-tos/use-copilot-agents/cloud-agent/use-cloud-agent-on-github), [GitHub Blog - "60 million Copilot code reviews and counting"](https://github.blog/ai-and-ml/github-copilot/60-million-copilot-code-reviews-and-counting/)

**Yetenekler**:
- Cloud agent **testleri çalıştırır**, linter'ları execute eder
- Başarısız workflow'ları analiz eder, fix üretir ("Fix with Copilot" butonu)
- PR merge box'ında "Approve and run workflows" butonu ile CI'yi tetikleme kontrolü
- Otomatik CI çalıştırma ayarlanabilir (Configuring settings for GitHub Copilot cloud agent)

**Test Generation (GenTest)**:
- GitHub Next prototipiydi, TestPilot olarak open source bırakıldı
- PR değişikliklerini analiz edip eksik testleri önerme
- Şu an doğrudan entegre değil, ayrı araç olarak mevcut

---

## Arkadaki Mimari ve Çalışma Prensipleri

### 3.1 Agentic Mimari

**Kaynak**: [GitHub Blog - "Copilot code review now runs on an agentic architecture"](https://github.blog/changelog/2026-03-05-copilot-code-review-now-runs-on-an-agentic-architecture), [GitHub Blog - "60 million Copilot code reviews and counting"](https://github.blog/ai-and-ml/github-copilot/60-million-copilot-code-reviews-and-counting/)

**Eski Mimari (Pre-March 2026)**:
- Shallow, line-by-line diff analizi
- Generic yorumlar, context eksikliği
- "Look at diff and comment" yaklaşımlı

**Yeni Agentic Mimari (Mart 2026+)**:
- **Agentic tool-calling**: LLM araçları çağırarak repo'yu keşfeder
- **Context gathering**: İlgili kodları, dizin yapısını, referansları aktif okur
- **Plan-driven**: Uzun PR'ler için review stratejisi haritalar
- **Memory**: Reviewler arası context paylaşımı (Copilot Memory ile)
- **Linked context**: Issue/PR linklerini takip eder, requirements uygunluğunu kontrol eder

**Sonuçlar**:
- %8.1 pozitif feedback artışı
- Daha az gürültü, daha yüksek sinyal
- Uzun/complex PR'lerde performans artışı

---

### 3.2 GitHub Actions Runner Kullanımı

**Kaynak**: [GitHub Docs - "Configuring runners for GitHub Copilot code review"](https://docs.github.com/enterprise-cloud@latest/copilot/how-tos/copilot-on-github/set-up-copilot/configure-runners), [GitHub Docs - "About GitHub Copilot code review"](https://docs.github.com/copilot/concepts/agents/code-review)

**Temel Prensipler**:
- Agentic yetenekler (context gathering, cloud agent handoff) **GitHub Actions runner'larında** çalışır
- Default: **GitHub-hosted Ubuntu x64 runners**
- Actions dakikaları tüketilir (private repo'larda)
- Self-hosted runner destekli: **Sadece ARC (Actions Runner Controller) ile**
- Larger runners (4-core, 8-core, GPU) performans için kullanılabilir

**Runner Yapılandırması**:
```yaml
# .github/workflows/copilot-code-review.yml
jobs:
  copilot-setup-steps:
    runs-on: arc-scale-set-name  # veya ubuntu-4-core für larger runners
```

**Self-Hosted Runner Gereksinimleri**:
- Ubuntu x64 Linux zorunlu
- ARC scale set kurulu olmalı
- Firewall: `api.githubcopilot.com`, `uploads.github.com`, `user-images.githubusercontent.com` + standart GH Actions host'ları
- Organization level'da default runner set edilebilir, repo override edebilir (veya kilitlenebilir)

**Fallback**: GitHub Actions kullanılamazsa / runner fail olursa → **Limited review** (agentic yetenekler olmadan) yine üretilir

---

### 3.3 Context Toplama (Context Gathering)

**Kaynak**: [DEV Community - "GitHub Copilot Code Review: Complete Guide (2026)"](https://dev.to/rahulxsingh/github-copilot-code-review-complete-guide-2026-255h), [GitHub Blog - "60 million Copilot code reviews and counting"](https://github.blog/ai-and-ml/github-copilot/60-million-copilot-code-reviews-and-counting/)

**Süreç**:
1. **Diff analizi**: Değişen dosyalar, satırlar
2. **Repository exploration**: Tool calling ile ilgili dosyaları okuma (imports, dependencies, testler)
3. **Architectural understanding**: Module boundaries, patterns, conventions
4. **Linked context**: Issue açıklamaları, bağlı PR'ler, discussion'lar
5. **Custom instructions**: Repository/agent/path-specific kurallar
6. **Copilot Memory**: Önceki review/agent oturumlarından öğrenilen fakteler

**Sınırlamalar**:
- Model context window'u ve time budget sınırları var
- 500+ satır, onlarca dosya olan PR'lerde kalite düşebilir
- Monorepo'larda deep dependency chain'ler tam trace edilemeyebilir

---

### 3.4 Copilot Memory

**Kaynak**: [GitHub Docs - "About GitHub Copilot Memory"](https://docs.github.com/copilot/concepts/agents/copilot-memory)

**İki Bellek Türü**:

| Tür | Kapsam | Oluşum | Kullanım |
|-----|--------|--------|----------|
| **Repository-level facts** | Repo bazlı, herkes tarafından kullanılabilir | Write access + Memory enabled kullanıcı aksiyonları | Kod convention'ları, mimari kararlar, database pattern'leri |
| **User-level preferences** | Kullanıcı bazlı, sadece o kullanıcı | Kullanıcı etkileşimleri | Kodlama stili, workflow tercihleri |

**Özellikler**:
- Citation (kanıt) ile saklanır, branch'e karşı validate edilir
- 28 gün kullanılmazsa auto-silinir (kullanıldığında timer reset)
- Kapalı PR'lardan da fact öğrenilebilir (validation geçerse)
- Cross-feature: Cloud agent öğrendiklerini code review kullanır ve tersi
- Admin export/delete edebilir (Business/Enterprise)

---

### 3.5 Model Kullanımı

**Kaynak**: [GitHub Docs - "About GitHub Copilot code review"](https://docs.github.com/copilot/concepts/agents/code-review)

- **Purpose-built model mix**: Copilot code review için özel tune edilmiş model karmaşıkı
- **Model switching desteklenmez**: Değişiklik güvenilirlik, UX, kaliteyi bozar
- "Models" settings sayfası **sadece Copilot Chat'i** kontrol eder, code review'i etkilemez
- GA (General Availability) terimlerine tabi

---

## Özelleştirme ve Yapılandırma

### 4.1 Custom Instructions

**Kaynak**: [GitHub Docs - "Using custom instructions to unlock the power of Copilot code review"](https://docs.github.com/en/copilot/tutorials/customize-code-review), [GitHub Blog - "Copilot code review and coding agent now support agent-specific instructions"](https://github.blog/changelog/2025-11-12-copilot-code-review-and-coding-agent-now-support-agent-specific-instructions)

**Dosya Türleri ve Kapsam**:

| Dosya | Konum | Kapsam | Aktivasyon |
|-------|-------|--------|------------|
| `copilot-instructions.md` | `.github/` | Repo-genel, Copilot-specific | Otomatik |
| `*.instructions.md` | `.github/instructions/**/` | Path-specific (applyTo frontmatter) | Değişen dosyalar eşleşirse |
| `AGENTS.md` | Repo root | Cross-tool/agent-agnostic | Otomatik |
| `SKILL.md` | `.github/skills/<skill>/` | Task-specific, on-demand | İlgili task'ta auto-invoke |

**Frontmatter Özellikleri**:
```markdown
---
applyTo: "**/*.py"          # Path-specific için
excludeAgent: "code-review" # Bu agent'ı hariç tut
excludeAgent: "coding-agent"# Bu agent'ı hariç tut
---
```

**Best Practices** (GitHub Docs):
- Dosya başına **max ~1000 satır** (daha fazlası kalite düşürür)
- Kısa, net, specific olmalı
- **Concrete examples** (doğru/yanlış kod snippet'leri) kritik
- Section'lar: Purpose, Naming, Code Style, Error Handling, Security, Testing, Performance
- Vague instructions ("be more accurate") işe yaramaz
- External link follow **desteklenmez** (içeriği kopyalayın)

**Head Branch Kuralı**: Copilot, instruction dosyalarını **head branch'ten** (değişiklik branch'i) okur, base branch'ten değil. Bu sayede instruction değişikliklerini aynı PR'de test edebilirsiniz.

---

### 4.2 Agent Skills

**Kaynak**: [GitHub Docs - "Adding agent skills for GitHub Copilot"](https://docs.github.com/en/copilot/how-tos/copilot-on-github/customize-copilot/customize-cloud-agent/add-skills), [GitHub Docs - "About agent skills"](https://docs.github.com/copilot/concepts/agents/about-agent-skills)

**Skill Yapısı**:
```
.github/skills/<skill-name>/
├── SKILL.md           # Zorunlu, YAML frontmatter + Markdown body
├── script.sh          # Opsiyonel, allowed-tools ile pre-approve
└── resources/         # Opsiyonel ek dosyalar
```

**SKILL.md Örneği**:
```markdown
---
name: github-actions-failure-debugging
description: Guide for debugging failing GitHub Actions workflows. Use this when asked to debug failing GitHub Actions workflows.
allowed-tools: shell  # opsiyonel, dikkatli kullanın
---
To debug failing GitHub Actions workflows in a pull request:
1. Use `list_workflow_runs` tool to look up recent workflow runs...
2. Use `summarize_job_log_failures` tool...
...
```

**Code Review İçin Skill**:
- Skill description/name **`code-review`** içermeli (review-focused skill olarak tanınır)
- Copilot karar verir: prompt + skill description'a göre load eder mi?
- **Custom instructions vs Skills**: Instructions = her task için basit kurallar; Skills = karmaşık, özel workflow'lar için

**CLI Yönetimi**: `gh skill search|preview|install|update|publish` (v2.90.0+)

---

### 4.3 Review Effort Level (Lite vs Balanced)

**Kaynak**: [GitHub Blog - "Copilot code review effort levels are generally available"](https://github.blog/changelog/2026-08-07-copilot-code-review-effort-levels-are-generally-available), [GitHub Docs - "About GitHub Copilot code review"](https://docs.github.com/copilot/concepts/agents/code-review)

| Seviye | Açıklama | Model | Maliyet (AI Credits) | Actions Dakikası |
|--------|----------|-------|---------------------|------------------|
| **Lite** | Standard review, rutin değişiklikler için | Standard reasoning | $0.05 - $1.00 | Daha az |
| **Balanced** | Derin analiz, security-sensitive, cross-service | Higher-reasoning model | $0.25 - $5.00 | Biraz daha fazla |

**Seçim/Yapılandırma**:
- Manual review: PR Reviewers bölümünde Copilot'ın yanında dropdown'dan seçim
- Automatic review: Organization owner default set eder, repo admin override edebilir
- Overview comment'te hangi level kullanıldığı gösterilir
- **Ağustos 2026 GA**: Low→Lite, Medium→Balanced olarak yeniden adlandırıldı

---

### 4.4 Otomatik Kod İncelemesi Yapılandırması

**Kaynak**: [GitHub Docs - "Configuring code review by GitHub Copilot"](https://docs.github.com/copilot/how-tos/copilot-on-github/set-up-copilot/configure-automatic-review)

**Yapılandırma Seviyeleri**:
1. **Repository Settings** → Code, planning, automation → Copilot → Code review
2. **Organization Settings** → Copilot → Code review (tüm repo'lar için default)
3. **Ruleset** (Branch protection): "Require Copilot code review" kuralı ekleme

**Ayarlar**:
- Automatic review: **Enabled/Disabled**
- Review effort level default: Lite / Balanced
- Auto-approval: Copilot onay verip vermesin (Count toward merge requirements)
- Trigger: Her push'ta review mi, sadece ilk açılışta mı?
- License olmayan kullanıcılar için: Organization policy "Allow members without a Copilot license to use Copilot code review" + "Enable Copilot code review" → herkes kullanabilir (billing org'a)

---

## Maliyet ve Faturalandırma

**Kaynak**: [GitHub Docs - "About GitHub Copilot code review"](https://docs.github.com/copilot/concepts/agents/code-review)

**İki Maliyet Bileşeni**:
1. **AI Credits** (Model etkileşimi - review'in kendisi)
   - Lite: ~$0.05 - $1.00/review
   - Balanced: ~$0.25 - $5.00/review
   - PR büyüklüğü, custom instructions ile artar

2. **GitHub Actions Minutes** (Agentic capabilities - context gathering, tool use)
   - Private repo'larda plan dakikalarından düşer
   - Self-hosted runner (ARC) dakika tüketmez
   - Larger runners daha yüksek dakika maliyeti

**Bütçe Kontrolleri** (Business/Enterprise):
- User-level budget dolduğunda → code review bloklanır
- Org/Enterprise spending limit dolduğunda → tüm AI credits features bloklanır
- License olmayan kullanıcıların harcaması → org/enterprise'a "additional usage" olarak yansır

---

## Sınırlılıklar ve Bilinen Sorunlar

**Kaynak**: [GitHub Docs - "About GitHub Copilot code review"](https://docs.github.com/copilot/concepts/agents/code-review), [GitHub Community Discussions](https://github.com/orgs/community/discussions/178108), [DEV Community Guide](https://dev.to/rahulxsingh/github-copilot-code-review-complete-guide-2026-255h)

| Sınırlılık | Açıklama |
|------------|----------|
| **Context Window** | Çok büyük PR'lerde (500+ satır, 50+ dosya) kalite düşer, subset of files review edilir |
| **No CI/CD Integration** | Code review, CI sonuçlarını (test coverage, build status) **görmez**; ayrı entegrasyon yok |
| **Custom Instructions Sync** | Otomatik review'lerde custom instructions bazen ignore edilir (cache sorunu); workaround: Copilot'ı reviewer'dan kaldırıp tekrar ekle |
| **Model Fixed** | Model değiştirilemez, tuning sadece GitHub tarafında |
| **No External Links** | Instruction dosyalarında external URL follow edilmez |
| **No PR Blocking** | "Tüm Copilot yorumları address edilmeden merge etme" kuralı koyulamaz |
| **No Format Control** | Review comment formatını (bold, emoji, sections) değiştiremezsiniz |
| **Self-hosted Runner** | Sadece ARC desteklenir, standart self-hosted **güvenlik nedeniyle desteklenmez** |
| **Mobile/IDE** | License olmayan kullanıcılar IDE'de code review kullanamaz (sadece web) |
| **Autofix Coverage** | Her CodeQL alert'ı için fix üretilmez; false positive/karmaşık durumlarda manual gerekir |

---

## Kaynaklar

### Resmi GitHub Dokümantasyonu
1. [About GitHub Copilot code review](https://docs.github.com/copilot/concepts/agents/code-review)
2. [Using GitHub Copilot code review](https://docs.github.com/copilot/using-github-copilot/code-review/using-copilot-code-review)
3. [Creating a pull request summary with GitHub Copilot](https://docs.github.com/enterprise-cloud@latest/copilot/github-copilot-enterprise/copilot-pull-request-summaries/creating-a-pull-request-summary-with-github-copilot)
4. [Configuring runners for GitHub Copilot code review](https://docs.github.com/enterprise-cloud@latest/copilot/how-tos/copilot-on-github/set-up-copilot/configure-runners)
5. [Configuring code review by GitHub Copilot](https://docs.github.com/copilot/how-tos/copilot-on-github/set-up-copilot/configure-automatic-review)
6. [Using Copilot cloud agent on GitHub](https://docs.github.com/en/copilot/how-tos/use-copilot-agents/cloud-agent/use-cloud-agent-on-github)
7. [About GitHub Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent)
8. [About GitHub Copilot Memory](https://docs.github.com/copilot/concepts/agents/copilot-memory)
9. [Using custom instructions to unlock the power of Copilot code review](https://docs.github.com/en/copilot/tutorials/customize-code-review)
10. [Adding agent skills for GitHub Copilot](https://docs.github.com/en/copilot/how-tos/copilot-on-github/customize-copilot/customize-cloud-agent/add-skills)
11. [Copilot Autofix for code scanning (Preview)](https://learn.microsoft.com/en-us/azure/devops/repos/security/github-advanced-security-code-scanning-autofix?view=azure-devops)

### GitHub Blog / Changelog
12. [Copilot code review now runs on an agentic architecture](https://github.blog/changelog/2026-03-05-copilot-code-review-now-runs-on-an-agentic-architecture) (Mar 2026)
13. [Ask @copilot to resolve merge conflicts on pull requests](https://github.blog/changelog/2026-03-26-ask-copilot-to-resolve-merge-conflicts-on-pull-requests) (Mar 2026)
14. [Fix merge conflicts in three clicks with Copilot cloud agent](https://github.blog/changelog/2026-04-13-fix-merge-conflicts-in-three-clicks-with-copilot-cloud-agent) (Apr 2026)
15. [Copilot Chat improvements for pull requests](https://github.blog/changelog/2026-04-23-copilot-chat-improvements-for-pull-requests) (Apr 2026)
16. [Copilot code review effort levels are generally available](https://github.blog/changelog/2026-08-07-copilot-code-review-effort-levels-are-generally-available) (Aug 2026)
17. [Copilot code review: Resolution reasons and expanded capabilities](https://github.blog/changelog/2026-08-27-copilot-code-review-resolution-reasons-and-expanded-capabilities) (Aug 2026)
18. [60 million Copilot code reviews and counting](https://github.blog/ai-and-ml/github-copilot/60-million-copilot-code-reviews-and-counting/) (Jan 2026)
19. [Copilot code review and coding agent now support agent-specific instructions](https://github.blog/changelog/2025-11-12-copilot-code-review-and-coding-agent-now-support-agent-specific-instructions) (Nov 2025)

### GitHub Next / Research
20. [Copilot for Pull Requests](https://githubnext.com/projects/copilot-for-pull-requests) (Technical preview, Dec 2023 sunset)

### Microsoft / Visual Studio
21. [Let GitHub Copilot draft your pull request description](https://devblogs.microsoft.com/visualstudio/let-github-copilot-draft-of-your-pull-request-description/) (Jul 2024)

### Topluluk / Üçüncü Taraf Analiz
22. [GitHub AI Code Review: 8 Copilot PR Automation Features](https://www.augmentcode.com/tools/github-copilot-ai-code-review) (AugmentCode analizi)
23. [GitHub Copilot Code Review: Complete Guide (2026)](https://dev.to/rahulxsingh/github-copilot-code-review-complete-guide-2026-255h) (DEV Community)
24. [GitHub Community Discussion - Custom instructions for PR review](https://github.com/orgs/community/discussions/178108)

---

## Özet

GitHub Copilot, PR yaşam döngüsünün **baştan sona** (açıklama yazma → özet çıkarma → kod incelemesi → inline fix uygulama → @copilot sohbeti ile iterasyon → merge conflict çözme → güvenlik fix'i → CI onayı → merge) **her aşamasında** entegre yetenekler sunar.

**Kritik Mimarî Noktalar**:
1. **Agentic architecture** (Mart 2026+): Tool-calling ile aktif context gathering
2. **GitHub Actions runner** zorunluluğu: Agentic yetenekler runner'da çalışır, self-hosted için ARC şart
3. **Copilot Memory**: Cross-feature (review ↔ cloud agent ↔ CLI) öğrenme ve paylaşım
4. **Custom Instructions + Agent Skills**: Repository/team standartlarını enjekte etmenin iki yolu
5. **Two effort levels**: Lite (hızlı/ucuz) vs Balanced (derin/pahalı) - per-review seçilebilir

**En Büyük Değer**: "60 million reviews" bloguna göre **71% review'lerde actionable feedback**, ortalama **5.1 yorum**, ve **%8.1 pozitif feedback artışı** agentic mimari ile elde edildi. WEX case study'sinde default AI review ile **%30 daha fazla kod ship edildi** rapor ediliyor.

---

*Rapor Tarihi: 2026-09-19*  
*Hazırlayan: Hermes Agent (Web araştırması ve resmi kaynaklardan derleme)*  
*Dosya: `/home/hermes/pr-review-go/docs/copilot_pr_capabilities.md`*