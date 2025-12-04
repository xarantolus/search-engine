<template>
  <div class="search-result-container">
    <div class="search-result">
      <a :href="enhance(result.url)" target="_blank" class="main-link">
        <h2 class="title" :title="removeMark(result.title)" v-html="sanitize(result._formatted?.title ?? result.title, false)"></h2>
        <p class="slug" :title="removeMark(result.slug)" v-html="sanitize(result._formatted?.slug ?? result.slug, false)"></p>
        <div v-if="result.isCode" class="code-snippet">
          <pre v-html="highlightCode(result.content)"></pre>
        </div>
        <p v-else-if="content.snippet" v-html="content.snippet" class="content markdown"></p>
      </a>
      <a :href="'/api/v1/text/' + result.id" target="_blank" class="date" title="Click to open full text">{{ formatDate(result.lastModified, true) }}</a>
    </div>
    <div class="icon-container">
      <a v-if="result.inFolderUrl" class="info-icon darkmode-inverted folder-icon" :href="result.inFolderUrl" target="_blank" title="Open in folder">
        <img src="/open-folder.svg" alt="Open in folder" />
      </a>
      <a v-if="aiEnabled && updateQueryFunction" :href="'/?q=' + encodeURIComponent('similar:' + result.id)" class="info-icon similar-icon" title="Find similar documents" @click.prevent="updateQueryFunction('similar:' + result.id)">✨</a>
    </div>
  </div>
</template>


<script setup lang="ts">
import DOMPurify from 'dompurify';
import { marked } from 'marked';
import hljs from 'highlight.js/lib/core';
import cpp from 'highlight.js/lib/languages/cpp';
import rust from 'highlight.js/lib/languages/rust';
import cmake from 'highlight.js/lib/languages/cmake';
import python from 'highlight.js/lib/languages/python';

// Register languages for syntax highlighting
hljs.registerLanguage('cpp', cpp);
hljs.registerLanguage('rust', rust);
hljs.registerLanguage('cmake', cmake);
hljs.registerLanguage('python', python);
hljs.registerLanguage('meson', python);

export interface FormattedSearchResult {
  id: number
  title: string
  content: string
  lastModified: string
  slug: string
  url: string
}

// Based on Document from shared/doc/doc.go
export interface SearchResult {
  id: string
  url: string
  inFolderUrl: string | null
  title: string
  content: string
  lastModified: string
  slug: string
  isCode: boolean

  _formatted?: FormattedSearchResult
}

const props = defineProps<{
  result: SearchResult,
  aiEnabled: boolean,
  updateQueryFunction?: (query: string) => void
}>()

const isMarkdown = (content: string) => {
  // Simple heuristic to check if content has markdown indicators
  return /[#`*_\-\[\]\(\)]/.test(content);
}

const removeMark = (content: string) => {
  return content.replace(/<mark>/g, '').replace(/<\/mark>/g, '');
}

const sanitize = (content: string, markdown: boolean = true) => {
  // truncate any line longer than 500 chars
  content = content.replace(/^(.{500}).*$/gm, '$1...')

  // merge short lines (< 30 chars) together, but only they start with a word character
  content = content.replace(
    /(^|\n)([^\n]{1,30})(\n+|$)/g,
    (match, p1, p2, _p3) => {
      if (/^\w/.test(p2)) {
        return p1 + p2.trim() + ' ';
      }
      return match;
    }
  )
  if (markdown && isMarkdown(content)) {
    content = marked(content, {
      gfm: true,
      pedantic: false,
    }) as string;
  }

  return DOMPurify.sanitize(content, {
    ALLOWED_TAGS: [
      'mark', 'p', 'strong', 'em', 'ul', 'li', 'code', 'pre', 'blockquote', 'h1', 'h2', 'h3',
      'table', 'thead', 'tbody', 'tr', 'th', 'td', 'br'
    ],
  })
    // fix <mark> tags getting escaped in code blocks
    .replace(/&lt;(\/?)mark&gt;/g, '<$1mark>')
    // fix "</mark" tags that weren't closed (the > got cut off)
    .replace(/(<\/mark)(?!>)/g, '$1>')
    // Makes some weirdly rendered lists ok again
    .replace(/<li>\s*<p>/g, '<li>')
    .replace(/<\/p>\s*<\/li>/g, '</li>')
    // Remove lines that contain only whitespace
    .replace(/^\s*[\r\n]+/gm, '\n')
    // Place <br> elements instead of newlines, except when the elemnt is already a block element
    .replace(
      /(?<!<\/(?:p|li|blockquote|h1|h2|h3|table|thead|tbody|tr|th|td)>)\n+(?!<\/?(?:p|li|blockquote|h1|h2|h3|table|thead|tbody|tr|th|td)>)/g,
      '<br>'
    )
    // remove empty <p></p> tags
    .replace(/<p>\s*<\/p>/g, '')
    .replace(/\[\.\.\.\]\s*(<br>)?/g, '<p class="low-visibility">[...]</p>')
    .replace(/<p>\s*<\/p>/g, '')
  // Replace <br>\s*[...]<br> with <p class="low-visibility">[...]</p>
}


const findLongestMarkRange = (content: string): [number, number] => {
  let start = -1
  let end = -1
  let searchPos = 0

  while (true) {
    const markStart = content.indexOf('<mark>', searchPos)
    if (markStart < 0) {
      break
    }

    const markEnd = content.indexOf('</mark>', markStart)
    if (markEnd < 0) {
      break
    }

    let curStart = markStart
    let curEnd = markEnd + '</mark>'.length
    let nextPos = curEnd

    // Merge current and next <mark> if within 15 chars
    while (true) {
      const nextMarkStart = content.indexOf('<mark>', nextPos)
      if (nextMarkStart < 0 || (nextMarkStart - nextPos) >= 15) {
        break
      }

      const nextMarkEnd = content.indexOf('</mark>', nextMarkStart)
      if (nextMarkEnd < 0) {
        break
      }
      const mergedEnd = nextMarkEnd + '</mark>'.length
      if (mergedEnd > curEnd) {
        curEnd = mergedEnd
      }
      nextPos = mergedEnd
    }

    if ((curEnd - curStart) > (end - start)) {
      start = curStart
      end = curEnd
    }
    searchPos = nextPos
  }
  return [start, end]
}

const getHighlightedContent = (content: string) => {
  if (!content) {
    return { snippet: '', highlight: '' }
  }

  const [longestStart, longestEnd] = findLongestMarkRange(content)
  let highlight = ''
  if (longestStart >= 0 && longestEnd > longestStart) {
    // Extract text and remove <mark> tags
    highlight = content.slice(longestStart, longestEnd)
      .replace(/<mark>/g, '')
      .replace(/<\/mark>/g, '')
  }

  // Return full unmodified snippet
  return {
    snippet: sanitize(content),
    highlight
  }
}

const content = getHighlightedContent(props.result._formatted?.content ?? props.result.content)

const enhance = (url: string) => {
  // Add a #:~:text= to the URL to highlight the search term highlight
  if (content.highlight && content.highlight.length > 2) {
    return `${url}#:~:text=${encodeURIComponent(content.highlight)}`
  }
  return url
}

const cleanHighlightedContent = (content: string) => {
  // Remove <mark> tags from the content
  return content.replace(/<mark>/g, '').replace(/<\/mark>/g, '');
}

const knownExtensions: Record<string, string> = {
  'cpp': 'cpp',
  'c': 'cpp',
  'h': 'cpp',
  'hpp': 'cpp',
  'rs': 'rust',
  'py': 'python',
  'cmake': 'cmake',
  'meson': 'meson'
}

const highlightCode = (code: string) => {
  try {
    const language = cleanHighlightedContent(props.result.slug)
      .split('.')
      .pop()
      ?.toLowerCase();

    const highlighted = language && knownExtensions[language] ? hljs.highlight(code, { language: knownExtensions[language], ignoreIllegals: true }) : hljs.highlightAuto(code);
    return highlighted.value
      // Fix <mark> tags
      .replace(/&lt;mark&gt;/g, '<mark>').replace(/&lt;\/mark&gt;/g, '</mark>');
  } catch (error) {
    console.error('Error highlighting code:', error);
    return code.replace(/&lt;mark&gt;/g, '<mark>').replace(/&lt;\/mark&gt;/g, '</mark>');
  }
};

const formatDate = (timestamp: number | string, detailed: boolean = false) => {
  let ts = typeof timestamp === 'string' ? parseInt(timestamp) : timestamp
  const date = new Date(ts * 1000);
  const year = date.getFullYear();
  const month = ('0' + (date.getMonth() + 1)).slice(-2);
  const day = ('0' + date.getDate()).slice(-2);
  if (detailed) {
    const hours = ('0' + date.getHours()).slice(-2);
    const minutes = ('0' + date.getMinutes()).slice(-2);

    return `${year}-${month}-${day} ${hours}:${minutes}`
  } else {
    return `${year}-${month}-${day}`;
  }
}
</script>

<style>
@import 'highlight.js/styles/github.css';
@import 'highlight.js/styles/github-dark.css';

.main-link {
    padding: 15px;
    display:block;
    padding-bottom: 25px;
}

.low-visibility {
  color: var(--slug-color);
}

.search-result-container {
  position: relative;
  margin-bottom: 15px;
}

.search-result {
  position: relative;
  display: block;
  border-radius: 8px;
  border: 1px solid var(--border-color);
  background-color: var(--background-color);
  transition: background-color 0.3s, border-color 0.3s;
  box-shadow: 0 4px 8px rgba(0, 0, 0, 0.1);
  text-decoration: none;
  color: inherit;
  width: 100%;
  box-sizing: border-box;
}

.code-snippet pre {
  background-color: var(--hover-background-color);
  padding: 1em;
  border-radius: 4px;
  font-family: monospace;
  white-space: pre-wrap;
  overflow-x: auto;
}

.icon-container {
  position: absolute;
  top: 8px;
  right: 8px;
  display: flex;
  gap: 8px;
}

.info-icon {
  background: rgba(255, 131, 73, 0.1);
  border: none;
  border-radius: 50%;
  width: 28px;
  height: 28px;
  padding: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 14px;
  cursor: pointer;
  transition: background 0.3s, opacity 0.3s;
  box-sizing: border-box;
  color: var(--text-color);
}

.similar-icon {
  background: rgba(255, 131, 73, 0.1);
}

.folder-icon {
  background: rgba(0, 123, 255, 0.25);
}

.folder-icon img {
  width: 14px;
  height: 14px;
  color: currentColor;
}

.info-icon:hover {
  background: rgba(255, 131, 73, 0.2);
}

.slug {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  padding-bottom: 10px;
}

.date {
  position: absolute;
  bottom: 10px;
  right: 15px;
  color: var(--slug-color);
  margin: 0;
}

.search-result:hover {
  background-color: var(--hover-background-color);
  border-color: var(--hover-border-color);
}

.title {
  font-size: 1.2em;
  margin: 4px 0;
  font-weight: bold;
  color: var(--link-color);
  overflow: hidden;
  text-overflow: ellipsis;
}

.slug {
  font-size: 0.9em;
  color: var(--slug-color);
  margin: 5px 0;
}

.content {
  font-size: 14px;
  color: var(--text-color);
  overflow: hidden;
}

mark {
  color: inherit;
  background-color: var(--highlight-color);
}

:root {
  --background-color: #ffffff;
  --border-color: #ccc;
  --hover-background-color: #f0f0f0;
  --hover-border-color: #bbb;
  --link-color: #007bff;
  --text-color: #333;
  --slug-color: #555;
  --highlight-color: yellow;
}

@media (prefers-color-scheme: dark) {
  :root {
    --background-color: #1e1e1e;
    --border-color: #444;
    --hover-background-color: #333333;
    --hover-border-color: #555;
    --link-color: #4ea8ff;
    --text-color: #ddd;
    --slug-color: #aaa;
    --highlight-color: #ffcc00;
  }

  mark {
    color: #333;
  }
}

.markdown {
  font-size: 1em;
}

.markdown h1,
.markdown h2,
.markdown h3 {
  margin: 0.5em 0;
  font-size: 1rem;
}

.markdown p {
  margin: 0.25em 0;
}

.markdown strong {
  font-weight: bold;
}

.markdown em {
  font-style: italic;
}

.markdown ul {
  list-style: inside disc;
  margin-left: 0.25em;
  padding-left: 0.5em;
}

.markdown li {
  margin: 0.25em 0;
}

.markdown code {
  background-color: var(--hover-background-color);
  padding: 0.2em 0.4em;
  border-radius: 4px;
  font-family: monospace;
  white-space: pre-wrap;
}

.markdown pre {
  background-color: var(--hover-background-color);
  padding: 1em;
  border-radius: 4px;
  font-family: monospace;
  white-space: pre-wrap;
  overflow-x: auto;
}

.markdown blockquote {
  border-left: 4px solid #ccc;
  padding-left: 0.5em;
  margin: 0.5em 0;
}

.markdown table {
  width: 100%;
  border-collapse: collapse;
  margin: 1em 0;
}
.markdown th,
.markdown td {
  border: 1px solid #ddd;
  padding: 0.5em;
  text-align: left;
}
.markdown th {
  font-weight: bold;
}
</style>
