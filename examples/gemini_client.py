from google import genai
from google.genai import types
from pathlib import Path

client = genai.Client(
    api_key="your-api-key",
    http_options={
        "base_url": "http://localhost:8787/gemini",
        "api_version": "v1beta",
    },
)

image_path = Path(__file__).with_name("fiber.png")

response = client.models.generate_content(
    model="gemini-advanced",
    contents=[
        types.Content(
            role="user",
            parts=[
                types.Part.from_text(text="Describe this image in detail."),
                types.Part.from_bytes(
                    data=image_path.read_bytes(),
                    mime_type="image/png",
                ),
            ],
        )
    ],
)

print(response.text)
