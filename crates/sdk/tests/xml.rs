//! Onbetrouwbare XML blijft begrensd en wordt precies één keer gedecodeerd.
#![allow(clippy::unwrap_used)]
use stulp_sdk::xml::Document;
#[test]
fn entities_cdata_and_qualified_names() {
    let d=Document::parse(br#"<?xml version='1.0'?><s:root xmlns:s='test'><a>&lt;track&gt; &amp;amp; &#x1F3B5;</a><b><![CDATA[<literal>]]></b></s:root>"#).unwrap();
    let root = d.find("root").unwrap();
    assert_eq!(d.field(root, "a"), "<track> &amp; 🎵");
    assert_eq!(d.field(root, "b"), "<literal>");
}
#[test]
fn rejects_entities_mismatches_multiple_roots_and_unbounded_depth() {
    for bytes in [
        "<!DOCTYPE root SYSTEM 'file:///etc/passwd'><root/>",
        "<a>&other;</a>",
        "<a>&#0;</a>",
        "<a></b>",
        "<a/><b/>",
        "<a k='1'k='2'/>",
        "<a k='1' k='2'/>",
        "<a><b>",
    ] {
        assert!(Document::parse(bytes.as_bytes()).is_err(), "{bytes}");
    }
    let deep = format!("{}{}", "<a>".repeat(65), "</a>".repeat(65));
    assert!(Document::parse(deep.as_bytes()).is_err());
}
