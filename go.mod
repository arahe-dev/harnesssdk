module github.com/arahe-dev/harnesssdk

go 1.26.0

// v1.6.0 made Result and Turn impossible to compare, breaking code written
// for v1.5; v1.7.0 restores them.
retract v1.6.0

require github.com/arahe-dev/hertasdk v1.0.0-next
