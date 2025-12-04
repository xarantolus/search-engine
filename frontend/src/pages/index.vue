<script setup lang="ts">
import { ref, onMounted, onUnmounted, watch, VNodeRef } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import SearchResult from '../components/SearchResult.vue'

type Sort = 'relevance' | 'newest' | 'oldest'
type FileType = 'include' | 'only-code' | 'ignore-code'

interface IndexStats {
	numberOfDocuments: number
	isIndexing: boolean
	fieldDistribution: Record<string, number>
}

interface Stats {
	stats: IndexStats,
	ai_enabled: boolean
}

const route = useRoute()
const router = useRouter()

// Reactive state for search
const query = ref<string>(route.query.q as string || '')
const sort = ref<Sort>((['relevance', 'newest', 'oldest'].includes(route.query.sort as string))
	? (route.query.sort as Sort) : 'relevance')
const fileTypes = ref<FileType>((['include', 'only-code', 'ignore-code'].includes(route.query.fileTypes as string))
	? (route.query.fileTypes as FileType) : 'include')
const filterOutEmpty = ref<boolean>(route.query.filterOutEmpty === 'true')
const sinceYear = ref<number | ''>(route.query.sinceYear ? parseInt(route.query.sinceYear as string) : '')
const aiRatio = ref<number>(route.query.aiRatio ? parseFloat(route.query.aiRatio as string) : 0)
const aiEnabled = ref<boolean>(false)

const results = ref<any[] | null>(null)
const totalResults = ref(0)
const processingTimeMS = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)
const offset = ref(0)
const limit = ref(25)
let abortController: AbortController | null = null

const stats = ref<Stats | null>(null)
const lastVisitNumber = ref<number | null>(null)

const badAverageScoreThreshold = 0.6
const loadedQuery = ref<string | null>(null)
let skipNextFetch: any = null;

function medianRankingScore(r: any[]) {
	if (!r.length) return Infinity
	const sortedScores = r.map(item => item._rankingScore).sort((a, b) => a - b)
	const mid = Math.floor(sortedScores.length / 2)
	return sortedScores.length % 2 !== 0 ? sortedScores[mid] : (sortedScores[mid - 1] + sortedScores[mid]) / 2
}

function isAlreadyAllQuoted(q: string) {
	const regex = /"([^"]+)"|(\S+)/g
	let match = regex.exec(q)
	while (match) {
		if (!match[1]) return false
		match = regex.exec(q)
	}
	return true
}

function quoteAllWordsInQuery(q: string) {
	const regex = /"([^"]+)"|(\S+)/g
	let match = regex.exec(q)
	let newQ = ''
	while (match) {
		newQ += `"${match[1] || match[2]}" `
		match = regex.exec(q)
	}
	return newQ.trim()
}

function buildRouterParams(override?: string) {
	const currentQuery: any = {}
	if ((override ?? query.value)?.trim()) currentQuery.q = override ?? query.value
	if (fileTypes.value !== 'include') currentQuery.fileTypes = fileTypes.value
	if (filterOutEmpty.value) currentQuery.filterOutEmpty = 'true'
	if (sinceYear.value) currentQuery.sinceYear = sinceYear.value.toString()

	if (isSimilarityQuery(currentQuery.q)) {
		return { query: currentQuery }
	}
	if (aiRatio.value !== 0 && sort.value === 'relevance') currentQuery.aiRatio = aiRatio.value.toString()
	if (sort.value !== 'relevance') currentQuery.sort = sort.value
	return { query: currentQuery }
}

function updateQuery(newQuery: string) {
	query.value = newQuery
	window.scrollTo({ top: 0, behavior: 'smooth' });
}

function isSimilarityQuery(q: string | null) : boolean {
	return q?.startsWith('similar:') ?? false
}

async function fetchResults() {
	if (abortController) {
		abortController.abort()
	}
	abortController = new AbortController()
	loading.value = true
	error.value = null

	try {
		const textQuery = query.value
		if (!textQuery.trim() && sort.value === 'relevance') {
			resetLoadedState()
			return
		}
		const res = await fetch('/api/v1/search', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({
				query: textQuery,
				limit: limit.value,
				offset: offset.value,
				sort: sort.value,
				fileTypes: fileTypes.value,
				ignoreEmptyFiles: filterOutEmpty.value,
				sinceYear: sinceYear.value === '' ? undefined : sinceYear.value,
				aiRatio: aiRatio.value
			}),
			signal: abortController.signal
		})
		if (!res.ok) {
			let dec : any;
			try {
				dec = await res.json()
			} catch (err: any) {
				console.error(err)
			}
			if (res.status === 403) {
				if (dec.group_error) {
					throw new Error(dec.message)
				}
			}
			if (dec.error) {
				throw new Error(dec.error)
			}

			return handleAuthError(res.status)
		}
		const newResults = await res.json()
		totalResults.value = newResults.estimatedTotalHits
		processingTimeMS.value = newResults.processingTimeMs
		results.value = offset.value === 0
			? newResults.hits
			: [...(results.value || []), ...newResults.hits]
		loadedQuery.value = textQuery
		const params = buildRouterParams();

		// let the watch() handler run, but skip the fetch
		skipNextFetch = params
		setTimeout(() => skipNextFetch = null, 100)
		router.push(params)

		loading.value = false
	} catch (err: any) {
		if (err.name !== 'AbortError') {
			error.value = "Error: " + (err.message || 'Something went wrong.')
			loading.value = false
		}
	}
}

let debounceTimeout: number | null = null
const debounce = (func: Function, delay: number) => {
	return (...args: any[]) => {
		if (debounceTimeout) {
			clearTimeout(debounceTimeout)
		}
		debounceTimeout = window.setTimeout(() => {
			func(...args)
		}, delay)
	}
}

const debouncedFetch = debounce(fetchResults, 150)

function handleAuthError(status: number) {
	if (status === 401 || status === 403) {
		location.href = '/login?redirect=' + encodeURIComponent(location.pathname + location.search)
	}
	throw new Error('Network response was not ok')
}

function resetLoadedState() {
	results.value = null
	loadedQuery.value = null
	loading.value = false
	totalResults.value = 0
	processingTimeMS.value = 0

	skipNextFetch = null
	router.push(buildRouterParams())
}

function loadMore() {
	offset.value += limit.value
	fetchResults()
}

function reset() {
	if (abortController) abortController.abort()
	abortController = null
	query.value = ''
	sort.value = 'relevance'
	fileTypes.value = 'include'
	filterOutEmpty.value = false
	sinceYear.value = ''
	aiRatio.value = 0 // reset the slider value
	offset.value = 0
	resetLoadedState()
	const searchInput = document.querySelector('.search-input') as HTMLInputElement
	if (searchInput) searchInput.focus()
}

async function fetchIndexStats() {
	const r = await fetch('/api/v1/stats')
	if (!r.ok) {
		handleAuthError(r.status)
	}
	const data: Stats = await r.json()
	return data
}

function sinceLastVisit() {
	if (lastVisitNumber.value && stats.value) {
		const diff = stats.value.stats.numberOfDocuments - lastVisitNumber.value
		return diff > 0 ? `+${diff}` : diff
	}
	return 0
}

const loadMoreButton = ref<HTMLElement | null>(null)
let observer: IntersectionObserver | null = null

function setupIntersectionObserver() {
	if (observer) {
		observer.disconnect()
		observer = null
	}
	if (!loadMoreButton.value) return
	observer = new IntersectionObserver(entries => {
		entries.forEach(e => { if (e.isIntersecting) loadMore() })
	}, { root: null, rootMargin: '0px', threshold: 0.75 })
	observer.observe(loadMoreButton.value)
}

watch([loadMoreButton], () => setupIntersectionObserver())

function deepEqual(x: any, y: any): boolean {
	// https://stackoverflow.com/a/32922084
	const ok = Object.keys, tx = typeof x, ty = typeof y;
	return x && y && tx === 'object' && tx === ty ? (
		ok(x).length === ok(y).length &&
		ok(x).every(key => deepEqual(x[key], y[key]))
	) : (x === y);
}

// Refetch when route changes from outside
watch(() => route.fullPath, () => {
	const isProgrammaticURLUpdate = deepEqual(skipNextFetch, buildRouterParams());
	skipNextFetch = null;

	if (isProgrammaticURLUpdate) {
		return
	}

	const q = route.query.q as string || ''
	const s = (['relevance', 'newest', 'oldest'].includes(route.query.sort as string))
		? route.query.sort as Sort : 'relevance'
	const ft = (['include', 'only-code', 'ignore-code'].includes(route.query.fileTypes as string))
		? route.query.fileTypes as FileType : 'include'
	const fo = route.query.filterOutEmpty === 'true'
	const sy = route.query.sinceYear ? parseInt(route.query.sinceYear as string) : ''
	const ar = route.query.aiRatio ? parseFloat(route.query.aiRatio as string) : 0

	query.value = q
	sort.value = s
	fileTypes.value = ft
	filterOutEmpty.value = fo
	sinceYear.value = sy
	aiRatio.value = ar
	offset.value = 0
	fetchResults()
})

// Update the watch list to include aiRatio
watch([query, sort, fileTypes, filterOutEmpty, sinceYear, aiRatio], async ([newQuery, newSort, newFileTypes, newFilterOutEmpty, newSinceYear, newAiRatio]) => {
	offset.value = 0
	debouncedFetch(newQuery, newSort, newFileTypes, newFilterOutEmpty, newSinceYear, newAiRatio)
})

const inputRef = ref<VNodeRef | null>(null)
function handleKeypress(e: KeyboardEvent) {
	if (e.key === '/' && inputRef.value !== document.activeElement) {
		e.preventDefault()
		inputRef.value?.focus()
	}
}

onMounted(async () => {
	window.addEventListener('keypress', handleKeypress)
	if (query.value || sort.value !== 'relevance') fetchResults()
	lastVisitNumber.value = parseInt(localStorage.getItem('lastVisitStat') || '0') || null
	aiEnabled.value = localStorage.getItem('aiEnabled') === 'true'

	const updateStats = async () => {
		try {
			stats.value = await fetchIndexStats()
		} catch (err: any) {
			console.error(err)
		}
		return stats.value
	}

	const st = await updateStats()
	if (st) localStorage.setItem('lastVisitStat', st.stats.numberOfDocuments.toString())
	setInterval(updateStats, 60000)

	aiEnabled.value = st?.ai_enabled ?? false
	localStorage.setItem('aiEnabled', aiEnabled.value.toString())
})

onUnmounted(() => {
	window.removeEventListener('keypress', handleKeypress)
	if (abortController) abortController.abort()
	if (observer) observer.disconnect()
})

const appName = (window as any).appName as unknown as string || 'Search';

const exportMessageOverride = ref<string>();
let exportMessageTimeout: number | null = null;
async function exportResults() {
	if (!results.value || results.value.length === 0) return

	let markdownList = results.value.map(result => {
		let title = result.title || result.slug || 'Untitled'
		if (result.title.length < 25) {
			title += " - " + result.slug
		}
		const url = result.url || result.slug || ''
		return `- [${title}](${url})`
	}).join('\n')

	// remove <mark> tags from the markdown list
	markdownList = markdownList.replace(/<mark>(.*?)<\/mark>/g, '$1')

	const markdownContent = `# Search Results: ${loadedQuery.value || 'query'}

${markdownList}

*Exported ${results.value.length} results on ${new Date().toLocaleDateString()}*`

	if (exportMessageTimeout) {
		clearTimeout(exportMessageTimeout)
	}
	exportMessageTimeout = setTimeout(() => {
		exportMessageOverride.value = undefined
	}, 2000)

	try {
		await navigator.clipboard.writeText(markdownContent)

		exportMessageOverride.value = 'Copied to clipboard!'
	} catch (err) {
		// Try workaround for older browsers (exec)
		const textArea = document.createElement('textarea')
		textArea.value = markdownContent
		document.body.appendChild(textArea)
		textArea.select()
		try {
			document.execCommand('copy')
			exportMessageOverride.value = 'Copied to clipboard!'
		} catch (copyErr) {
			exportMessageOverride.value = 'Failed to access clipboard!'
		} finally {
			document.body.removeChild(textArea)
		}
	}
}
</script>

<template>
	<h1 id="reset" @click="reset">{{ appName }}</h1>
	<p id="stats" :style="{ visibility: stats ? 'visible' : 'hidden' }">
		Search {{ stats?.stats.numberOfDocuments }} documents
		<template v-if="lastVisitNumber && lastVisitNumber !== stats?.stats.numberOfDocuments">
			({{ sinceLastVisit() }} since your last visit)
		</template>
	</p>
	<div class="search-container">
		<input ref="inputRef" autofocus v-model="query" type="text" placeholder="Search..." class="search-input" />
		<select v-model="sort" class="sort-dropdown" :disabled="isSimilarityQuery(loadedQuery)">
			<option value="relevance">Relevance</option>
			<option value="newest">Newest</option>
			<option value="oldest">Oldest</option>
		</select>
		<select v-model="fileTypes" class="sort-dropdown">
			<option value="include">All file types</option>
			<option value="only-code">Only Code files</option>
			<option value="ignore-code">Filter out Code</option>
		</select>
		<select v-model="filterOutEmpty" class="sort-dropdown">
			<option :value="false">Include no-text files</option>
			<option :value="true">Filter no-text files</option>
		</select>
		<select v-model="sinceYear" class="sort-dropdown">
			<option value="">Since forever</option>
			<option v-for="year in Array.from({ length: new Date().getFullYear() - 2000 + 1 }, (_, i) => new Date().getFullYear() - i)" :key="year" :value="year">
				Since {{ year }}
			</option>
		</select>
		<button @click="fetchResults" class="search-button" :disabled="loading" tabindex="-1">
			<span v-if="!loading">Search</span>
			<div v-else class="spinner"></div>
		</button>
	</div>
	<div v-if="aiEnabled" class="slider-container" :style="{ display: (sort === 'relevance' && !isSimilarityQuery(loadedQuery)) ? 'block' : 'none' }">
		<label for="aiRatio">Semantic Search Ratio ✨</label>
		<input id="aiRatio" type="range" v-model.number="aiRatio" min="0" max="1" step="0.01" />
	</div>
	<div class="error" :style="{ visibility: error ? 'visible' : 'hidden' }">
		{{ error }}
	</div>
	<div v-if="loadedQuery && results && medianRankingScore(results) < badAverageScoreThreshold && !isAlreadyAllQuoted(loadedQuery)">
		<p>
			Results are not very relevant. <RouterLink :to="buildRouterParams(quoteAllWordsInQuery(loadedQuery))">
				Search with quotes
			</RouterLink>
			for more precise results.
		</p>
	</div>
	<div v-if="totalResults && results" class="total-results" :title="`Ranking score is ${medianRankingScore(results).toFixed(3)}`">
		<div class="results-header">
			<span>{{ totalResults }} results in {{ processingTimeMS }}ms. </span>
			<a style="cursor: pointer;" @click.prevent="exportResults">{{ exportMessageOverride ?? 'Export' }}</a>.
		</div>
		<br>
		<RouterLink v-if="!isSimilarityQuery(loadedQuery) && medianRankingScore(results) >= badAverageScoreThreshold && loadedQuery && !isAlreadyAllQuoted(loadedQuery) && !medianRankingScore(results).toFixed(3).startsWith('1.')" :to="buildRouterParams(quoteAllWordsInQuery(loadedQuery))">Improve accuracy</RouterLink>
	</div>
	<div v-if="results !== null" class="results-container">
		<SearchResult :ai-enabled="aiEnabled" v-if="results.length" v-for="r in results" :key="r.id + loadedQuery" :result="r" :update-query-function="updateQuery" />
		<div v-else class="no-results">
			<p class="no-results-message">
				No results found. Please try a different search term.
			</p>
		</div>
	</div>
	<div v-else-if="!loading && query && !error" class="error">
		No results found
	</div>
	<button ref="loadMoreButton" v-if="results && results.length < totalResults" @click="loadMore" :disabled="loading" class="load-more">
		Load More
	</button>
</template>

<style>
.search-container {
	display: flex;
	align-items: center;
	gap: 2px;
	min-height: 50px;
}

.no-results {
	text-align: center;
}

.no-results-message {
	color: #ccc;
}

@media (max-width: 1000px) {
	.search-container {
		flex-direction: column;
		align-items: stretch;
	}

	.search-input {
		width: 100%;
	}

	.sort-dropdown,
	.search-button {
		width: 100%;
	}
}

#reset {
	cursor: pointer;
	margin-bottom: 0;
}

#stats {
	margin-top: 0;
	margin-bottom: 1.25em;
}

.sort-dropdown {
	padding: 12px;
	font-size: 16px;
	border: 1px solid #ccc;
	border-radius: 4px;
	box-sizing: border-box;
	height: 45px;
}

.search-input {
	flex: 1;
	padding: 12px;
	font-size: 16px;
	border: 1px solid #ccc;
	border-radius: 4px;
	box-sizing: border-box;
}

.search-button {
	position: relative;
	height: 45px;
	font-size: 16px;
	background-color: #007bff;
	color: #fff;
	border: 1px solid #ccc;
	border-radius: 4px;
	cursor: pointer;
	transition: background-color 0.3s;
	display: flex;
	align-items: center;
	justify-content: center;
	min-width: 100px;
}

.search-button:disabled {
	background-color: #6c757d;
	cursor: not-allowed;
}

.search-button:hover:not(:disabled) {
	background-color: #0056b3;
}

.spinner {
	width: 20px;
	height: 20px;
	border: 4px solid #fff;
	border-top: 4px solid #007bff;
	border-radius: 50%;
	animation: spin 1s linear infinite;
}

@keyframes spin {
	to {
		transform: rotate(360deg);
	}
}

.error {
	margin-top: 10px;
	color: red;
}

.total-results {
	margin-top: 10px;
	font-size: 14px;
	color: #555;
}

.results-container {
	text-align: left;
	margin-top: 20px;
}

.load-more {
	margin-top: 20px;
	padding: 10px 25px;
	font-size: 16px;
	border: none;
	border-radius: 4px;
	background-color: #007bff;
	cursor: pointer;
	transition: background-color 0.3s;
}

.load-more:hover:not(:disabled) {
	background-color: #0056b3;
}

.load-more:disabled {
	cursor: not-allowed;
}

.slider-container {
	display: flex;
	flex-direction: column;
	align-items: flex-start;
	gap: 4px;
	margin-top: 8px;
}

.slider-container input[type="range"] {
	width: 100%;
}
</style>
