group "default" {
  targets = ["searcher", "indexer"]
}

target "searcher" {
  platforms = ["linux/amd64", "linux/arm64"]
}


target "indexer" {
  platforms = ["linux/amd64", "linux/arm64"]
}
